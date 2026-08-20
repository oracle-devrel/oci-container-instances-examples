// OCI log forwarder for the sidecar sample.
//
// The forwarder tails a shared log file, persists offsets by inode so rotation
// can be drained safely, stages batches on disk, and ships them to OCI Logging
// by using the container instance resource principal.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/loggingingestion"
)

var logger = log.New(os.Stdout, "[log-forwarder] ", log.LstdFlags)

// trackedFile stores the current read position for one inode-backed file.
type trackedFile struct {
	Path   string `json:"path"`
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

type readBatch struct {
	SourcePath string   `json:"source_path"`
	Inode      uint64   `json:"inode"`
	EndOffset  int64    `json:"end_offset"`
	Lines      []string `json:"lines"`
}

type statePayload struct {
	TrackedFiles []trackedFile `json:"tracked_files"`
}

type spoolPayload struct {
	SourcePath string   `json:"source_path"`
	Inode      uint64   `json:"inode"`
	EndOffset  int64    `json:"end_offset"`
	Lines      []string `json:"lines"`
	CreatedAt  string   `json:"created_at"`
}

// spoolQueue persists pending batches before OCI API calls succeed.
type spoolQueue struct {
	spoolDir string
}

func newSpoolQueue(spoolDir string) (*spoolQueue, error) {
	if err := os.MkdirAll(spoolDir, 0o755); err != nil {
		return nil, err
	}
	return &spoolQueue{spoolDir: spoolDir}, nil
}

func (q *spoolQueue) recoverOffsets() []trackedFile {
	tracked := map[uint64]trackedFile{}
	for _, batchPath := range q.listBatches() {
		payload, err := q.readPayload(batchPath)
		if err != nil {
			logger.Printf("WARNING ignoring unreadable spool file %s: %v", batchPath, err)
			continue
		}
		existing, ok := tracked[payload.Inode]
		if !ok || payload.EndOffset > existing.Offset {
			tracked[payload.Inode] = trackedFile{
				Path:   payload.SourcePath,
				Inode:  payload.Inode,
				Offset: payload.EndOffset,
			}
		}
	}
	recovered := make([]trackedFile, 0, len(tracked))
	for _, item := range tracked {
		recovered = append(recovered, item)
	}
	return recovered
}

func (q *spoolQueue) count() int {
	return len(q.listBatches())
}

func (q *spoolQueue) listBatches() []string {
	matches, err := filepath.Glob(filepath.Join(q.spoolDir, "*.json"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func (q *spoolQueue) writeBatch(batch readBatch) error {
	randomHex := make([]byte, 8)
	if _, err := rand.Read(randomHex); err != nil {
		return err
	}
	payload := spoolPayload{
		SourcePath: batch.SourcePath,
		Inode:      batch.Inode,
		EndOffset:  batch.EndOffset,
		Lines:      batch.Lines,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	batchName := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(randomHex))
	tmpPath := filepath.Join(q.spoolDir, "."+batchName+".tmp")
	finalPath := filepath.Join(q.spoolDir, batchName)
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

func (q *spoolQueue) readBatch(path string) (readBatch, error) {
	payload, err := q.readPayload(path)
	if err != nil {
		return readBatch{}, err
	}
	return readBatch{
		SourcePath: payload.SourcePath,
		Inode:      payload.Inode,
		EndOffset:  payload.EndOffset,
		Lines:      append([]string(nil), payload.Lines...),
	}, nil
}

func (q *spoolQueue) readPayload(path string) (spoolPayload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return spoolPayload{}, err
	}
	var payload spoolPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return spoolPayload{}, err
	}
	return payload, nil
}

type fileTracker struct {
	path         string
	statePath    string
	readFromHead bool
	trackedFiles []trackedFile
}

func newFileTracker(path, statePath string, readFromHead bool, recoveredOffsets []trackedFile) (*fileTracker, error) {
	tracker := &fileTracker{
		path:         path,
		statePath:    statePath,
		readFromHead: readFromHead,
	}
	tracker.loadState()
	tracker.mergeRecoveredOffsets(recoveredOffsets)
	if err := tracker.ensureState(); err != nil {
		return nil, err
	}
	return tracker, nil
}

func (t *fileTracker) loadState() {
	data, err := os.ReadFile(t.statePath)
	if err != nil {
		return
	}
	var payload statePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		logger.Printf("WARNING ignoring unreadable state file %s: %v", t.statePath, err)
		return
	}
	t.trackedFiles = payload.TrackedFiles
}

func (t *fileTracker) mergeRecoveredOffsets(recoveredOffsets []trackedFile) {
	merged := map[uint64]trackedFile{}
	for _, tracked := range t.trackedFiles {
		merged[tracked.Inode] = tracked
	}
	for _, recovered := range recoveredOffsets {
		existing, ok := merged[recovered.Inode]
		if !ok {
			merged[recovered.Inode] = recovered
			continue
		}
		if recovered.Offset > existing.Offset {
			existing.Offset = recovered.Offset
		}
		if existing.Path != recovered.Path {
			existing.Path = recovered.Path
		}
		merged[recovered.Inode] = existing
	}
	t.trackedFiles = t.trackedFiles[:0]
	for _, tracked := range merged {
		t.trackedFiles = append(t.trackedFiles, tracked)
	}
}

func (t *fileTracker) persistState() error {
	if err := os.MkdirAll(filepath.Dir(t.statePath), 0o755); err != nil {
		return err
	}
	payload := statePayload{TrackedFiles: t.trackedFiles}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tmpPath := t.statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, t.statePath)
}

func (t *fileTracker) ensureLogFile() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	handle, err := os.OpenFile(t.path, os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	return handle.Close()
}

func (t *fileTracker) findPathByInode(inode uint64) string {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(t.path), filepath.Base(t.path)+"*"))
	if err != nil {
		return ""
	}
	for _, candidate := range matches {
		fileInfo, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		stat, ok := fileInfo.Sys().(*syscall.Stat_t)
		if ok && stat.Ino == inode {
			return candidate
		}
	}
	return ""
}

func (t *fileTracker) ensureState() error {
	if err := t.ensureLogFile(); err != nil {
		return err
	}
	currentInfo, err := os.Stat(t.path)
	if err != nil {
		return err
	}
	currentStat := currentInfo.Sys().(*syscall.Stat_t)
	currentInode := currentStat.Ino
	currentSize := currentInfo.Size()

	resolved := make([]trackedFile, 0, len(t.trackedFiles)+1)
	currentPresent := false

	for _, tracked := range t.trackedFiles {
		resolvedPath := tracked.Path
		fileInfo, err := os.Stat(resolvedPath)
		if err != nil {
			found := t.findPathByInode(tracked.Inode)
			if found == "" {
				logger.Printf("WARNING dropping unreadable tracked file inode=%d offset=%d path=%s", tracked.Inode, tracked.Offset, tracked.Path)
				continue
			}
			resolvedPath = found
			fileInfo, err = os.Stat(resolvedPath)
			if err != nil {
				continue
			}
		}
		stat := fileInfo.Sys().(*syscall.Stat_t)
		if stat.Ino != tracked.Inode {
			found := t.findPathByInode(tracked.Inode)
			if found == "" {
				logger.Printf("WARNING dropping unreadable tracked file inode=%d offset=%d path=%s", tracked.Inode, tracked.Offset, tracked.Path)
				continue
			}
			resolvedPath = found
			fileInfo, err = os.Stat(resolvedPath)
			if err != nil {
				continue
			}
		}
		tracked.Path = resolvedPath
		if tracked.Inode == currentInode {
			tracked.Path = t.path
			if tracked.Offset > currentSize {
				tracked.Offset = currentSize
			}
			currentPresent = true
		}
		resolved = append(resolved, tracked)
	}

	if len(resolved) == 0 {
		initialOffset := int64(0)
		if !t.readFromHead {
			initialOffset = currentSize
		}
		resolved = append(resolved, trackedFile{Path: t.path, Inode: currentInode, Offset: initialOffset})
		currentPresent = true
	}
	if !currentPresent {
		resolved = append(resolved, trackedFile{Path: t.path, Inode: currentInode, Offset: 0})
	}
	t.reorderTrackedFiles(resolved)
	return t.persistState()
}

func (t *fileTracker) reorderTrackedFiles(items []trackedFile) {
	rotated := make([]trackedFile, 0, len(items))
	current := make([]trackedFile, 0, 1)
	for _, item := range items {
		if item.Path == t.path {
			current = append(current, item)
			continue
		}
		rotated = append(rotated, item)
	}
	t.trackedFiles = append(rotated, current...)
}

func (t *fileTracker) refreshCurrentFile() error {
	if err := t.ensureLogFile(); err != nil {
		return err
	}
	currentInfo, err := os.Stat(t.path)
	if err != nil {
		return err
	}
	currentStat := currentInfo.Sys().(*syscall.Stat_t)
	currentInode := currentStat.Ino
	currentSize := currentInfo.Size()

	currentIndex := -1
	for i, tracked := range t.trackedFiles {
		if tracked.Path == t.path {
			currentIndex = i
			break
		}
	}
	if currentIndex >= 0 && t.trackedFiles[currentIndex].Inode != currentInode {
		found := t.findPathByInode(t.trackedFiles[currentIndex].Inode)
		if found != "" {
			t.trackedFiles[currentIndex].Path = found
		} else {
			logger.Printf("WARNING current file inode=%d moved but no rotated file was found; unread data may be lost", t.trackedFiles[currentIndex].Inode)
			t.trackedFiles = append(t.trackedFiles[:currentIndex], t.trackedFiles[currentIndex+1:]...)
		}
		currentIndex = -1
	}

	if currentIndex < 0 {
		existingIndex := -1
		for i, tracked := range t.trackedFiles {
			if tracked.Inode == currentInode {
				existingIndex = i
				break
			}
		}
		if existingIndex < 0 {
			t.trackedFiles = append(t.trackedFiles, trackedFile{Path: t.path, Inode: currentInode, Offset: 0})
		} else {
			t.trackedFiles[existingIndex].Path = t.path
			if t.trackedFiles[existingIndex].Offset > currentSize {
				t.trackedFiles[existingIndex].Offset = currentSize
			}
		}
	} else if currentSize < t.trackedFiles[currentIndex].Offset {
		logger.Printf("WARNING current file %s shrank from %d bytes to %d bytes; rewinding tracked offset", t.path, t.trackedFiles[currentIndex].Offset, currentSize)
		t.trackedFiles[currentIndex].Offset = currentSize
	}

	t.reorderTrackedFiles(t.trackedFiles)
	return t.persistState()
}

func (t *fileTracker) dropIfDrained(index int) (bool, error) {
	tracked := t.trackedFiles[index]
	if tracked.Path == t.path {
		return false, nil
	}

	fileInfo, err := os.Stat(tracked.Path)
	if err != nil {
		found := t.findPathByInode(tracked.Inode)
		if found == "" {
			logger.Printf("WARNING tracked rotated file inode=%d disappeared before it was fully consumed", tracked.Inode)
			t.trackedFiles = append(t.trackedFiles[:index], t.trackedFiles[index+1:]...)
			return true, t.persistState()
		}
		tracked.Path = found
		t.trackedFiles[index] = tracked
		fileInfo, err = os.Stat(found)
		if err != nil {
			return false, err
		}
	}

	if tracked.Offset >= fileInfo.Size() {
		t.trackedFiles = append(t.trackedFiles[:index], t.trackedFiles[index+1:]...)
		return true, t.persistState()
	}
	return false, nil
}

func (t *fileTracker) readBatch(maxLines int, maxBytes int) (*readBatch, error) {
	if err := t.refreshCurrentFile(); err != nil {
		return nil, err
	}

	for index := 0; index < len(t.trackedFiles); {
		drained, err := t.dropIfDrained(index)
		if err != nil {
			return nil, err
		}
		if drained {
			continue
		}

		tracked := t.trackedFiles[index]
		handle, err := os.Open(tracked.Path)
		if err != nil {
			found := t.findPathByInode(tracked.Inode)
			if found == "" {
				logger.Printf("WARNING tracked file inode=%d disappeared before it could be consumed", tracked.Inode)
				t.trackedFiles = append(t.trackedFiles[:index], t.trackedFiles[index+1:]...)
				if err := t.persistState(); err != nil {
					return nil, err
				}
				continue
			}
			t.trackedFiles[index].Path = found
			if err := t.persistState(); err != nil {
				return nil, err
			}
			return t.readBatch(maxLines, maxBytes)
		}

		if _, err := handle.Seek(tracked.Offset, io.SeekStart); err != nil {
			handle.Close()
			return nil, err
		}

		reader := bufio.NewReader(handle)
		lines := make([]string, 0, maxLines)
		totalBytes := 0
		currentOffset := tracked.Offset
		endOffset := tracked.Offset

		for len(lines) < maxLines {
			startOffset := currentOffset
			rawLine, err := reader.ReadBytes('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				handle.Close()
				return nil, err
			}
			if len(rawLine) == 0 && errors.Is(err, io.EOF) {
				break
			}

			currentOffset += int64(len(rawLine))
			line := strings.TrimRight(string(rawLine), "\r\n")
			lineBytes := len([]byte(line))
			if len(lines) > 0 && totalBytes+lineBytes > maxBytes {
				endOffset = startOffset
				break
			}

			lines = append(lines, line)
			totalBytes += lineBytes
			endOffset = currentOffset

			if errors.Is(err, io.EOF) {
				break
			}
		}

		handle.Close()

		if len(lines) > 0 {
			return &readBatch{
				SourcePath: tracked.Path,
				Inode:      tracked.Inode,
				EndOffset:  endOffset,
				Lines:      lines,
			}, nil
		}

		drained, err = t.dropIfDrained(index)
		if err != nil {
			return nil, err
		}
		if !drained {
			index++
		}
	}

	return nil, nil
}

func (t *fileTracker) markSpooled(batch readBatch) error {
	index := -1
	for i, item := range t.trackedFiles {
		if item.Inode == batch.Inode && item.Path == batch.SourcePath {
			index = i
			break
		}
	}
	if index < 0 {
		for i, item := range t.trackedFiles {
			if item.Inode == batch.Inode {
				index = i
				break
			}
		}
	}
	if index < 0 {
		t.trackedFiles = append(t.trackedFiles, trackedFile{
			Path:   batch.SourcePath,
			Inode:  batch.Inode,
			Offset: batch.EndOffset,
		})
	} else {
		t.trackedFiles[index].Path = batch.SourcePath
		if t.trackedFiles[index].Offset < batch.EndOffset {
			t.trackedFiles[index].Offset = batch.EndOffset
		}
	}
	t.reorderTrackedFiles(t.trackedFiles)
	return t.persistState()
}

type ociLogForwarder struct {
	client               loggingingestion.LoggingClient
	logID                string
	logSource            string
	logSubject           string
	logType              string
	flushInterval        time.Duration
	chunkLimitBytes      int
	maxQueuedBatches     int
	maxBatchEntries      int
	maxEntrySizeBytes    int
	pollInterval         time.Duration
	diskUsageLogInterval time.Duration
	retryInitial         time.Duration
	retryMax             time.Duration
	spoolQueue           *spoolQueue
	fileTracker          *fileTracker
	stopRequested        bool
	lastFlushAt          time.Time
	nextDiskUsageLogAt   time.Time
}

// newOciLogForwarder resolves environment variables and reconstructs any
// durable state that was left behind by earlier runs.
func newOciLogForwarder() (*ociLogForwarder, error) {
	client, err := buildLoggingClient()
	if err != nil {
		return nil, err
	}
	logFilePath := getenvRequired("LOG_FILE_PATH")
	stateDir := getenvDefault("LOG_FORWARDER_STATE_DIR", "/var/lib/oci-log-forwarder/state")
	spoolDir := getenvDefault("LOG_FORWARDER_SPOOL_DIR", "/var/lib/oci-log-forwarder/spool")
	statePath := getenvDefault("LOG_STATE_FILE", filepath.Join(stateDir, "input.json"))
	queueDir := getenvDefault("LOG_QUEUE_DIR", spoolDir)

	spoolQueue, err := newSpoolQueue(queueDir)
	if err != nil {
		return nil, err
	}
	recoveredOffsets := spoolQueue.recoverOffsets()
	fileTracker, err := newFileTracker(logFilePath, statePath, parseBool(getenvDefault("READ_FROM_HEAD", "true")), recoveredOffsets)
	if err != nil {
		return nil, err
	}

	return &ociLogForwarder{
		client:               client,
		logID:                getenvRequired("OCI_LOG_OBJECT_ID"),
		logSource:            getenvDefault("OCI_SOURCE", hostName()),
		logSubject:           getenvDefault("OCI_SUBJECT", logFilePath),
		logType:              getenvDefault("OCI_LOG_TYPE", "app.log"),
		flushInterval:        parseDuration(getenvDefault("LOG_FORWARDER_FLUSH_INTERVAL", "5s")),
		chunkLimitBytes:      parseSize(getenvDefault("LOG_FORWARDER_CHUNK_LIMIT_SIZE", "1m")),
		maxQueuedBatches:     mustAtoi(getenvDefault("LOG_FORWARDER_QUEUED_BATCH_LIMIT", "64")),
		maxBatchEntries:      mustAtoi(getenvDefault("OCI_MAX_BATCH_ENTRIES", "1000")),
		maxEntrySizeBytes:    mustAtoi(getenvDefault("OCI_MAX_ENTRY_SIZE_BYTES", "900000")),
		pollInterval:         parseDuration(getenvDefault("LOG_POLL_INTERVAL_SECONDS", "1")),
		diskUsageLogInterval: parseDuration(getenvDefault("LOG_FORWARDER_DISK_USAGE_LOG_INTERVAL", "5m")),
		retryInitial:         parseDuration(getenvDefault("OCI_RETRY_INITIAL_SECONDS", "1")),
		retryMax:             parseDuration(getenvDefault("OCI_RETRY_MAX_SECONDS", "30")),
		spoolQueue:           spoolQueue,
		fileTracker:          fileTracker,
	}, nil
}

// start runs the main poll, spool, and flush loop until shutdown is requested.
func (f *ociLogForwarder) start(ctx context.Context) int {
	logger.Printf("starting OCI log forwarder")
	logger.Printf("source file: %s", f.fileTracker.path)
	logger.Printf("OCI auth mode: resource_principal")
	logger.Printf("OCI log object id: %s", f.logID)
	f.logLogStorageUsageIfDue(true)

	for !f.stopRequested {
		select {
		case <-ctx.Done():
			logger.Printf("received signal; draining disk spool before exit")
			f.stopRequested = true
			continue
		default:
		}

		f.logLogStorageUsageIfDue(false)
		f.flushSpool(false)

		if f.spoolQueue.count() < f.maxQueuedBatches {
			batch, err := f.fileTracker.readBatch(f.maxBatchEntries, f.chunkLimitBytes)
			if err != nil {
				logger.Printf("ERROR log forwarder failed: %v", err)
				return 1
			}
			if batch != nil {
				for i, line := range batch.Lines {
					batch.Lines[i] = f.normalizeLine(line)
				}
				if err := f.spoolQueue.writeBatch(*batch); err != nil {
					logger.Printf("ERROR log forwarder failed: %v", err)
					return 1
				}
				if err := f.fileTracker.markSpooled(*batch); err != nil {
					logger.Printf("ERROR log forwarder failed: %v", err)
					return 1
				}
				continue
			}
		}

		time.Sleep(f.pollInterval)
	}

	f.flushSpool(true)
	return 0
}

func (f *ociLogForwarder) logLogStorageUsageIfDue(force bool) {
	if f.diskUsageLogInterval <= 0 {
		return
	}
	now := time.Now()
	if !force && !f.nextDiskUsageLogAt.IsZero() && now.Before(f.nextDiskUsageLogAt) {
		return
	}

	pattern := filepath.Join(filepath.Dir(f.fileTracker.path), filepath.Base(f.fileTracker.path)+"*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	var totalBytes int64
	fileCount := 0
	sort.Strings(matches)
	for _, candidate := range matches {
		fileInfo, err := os.Stat(candidate)
		if err != nil || !fileInfo.Mode().IsRegular() {
			continue
		}
		totalBytes += fileInfo.Size()
		fileCount++
	}

	logger.Printf(
		"log files consume %d (%s) across %d file(s) under %s",
		totalBytes,
		formatSizeBytes(totalBytes),
		fileCount,
		filepath.Dir(f.fileTracker.path),
	)
	f.nextDiskUsageLogAt = now.Add(f.diskUsageLogInterval)
}

func (f *ociLogForwarder) normalizeLine(line string) string {
	data := []byte(line)
	if len(data) <= f.maxEntrySizeBytes {
		return line
	}

	marker := " [truncated]"
	allowed := f.maxEntrySizeBytes - len([]byte(marker))
	if allowed < 0 {
		allowed = 0
	}
	truncated := data[:allowed]
	for len(truncated) > 0 && !utf8.Valid(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	logger.Printf("WARNING truncating oversized log entry from %d bytes to %d bytes", len(data), f.maxEntrySizeBytes)
	return string(truncated) + marker
}

// flushSpool retries queued batches with exponential backoff and keeps the
// oldest batch first to preserve ordering as much as possible.
func (f *ociLogForwarder) flushSpool(stopWhenEmpty bool) {
	backoff := f.retryInitial
	for {
		batchPaths := f.spoolQueue.listBatches()
		if len(batchPaths) == 0 {
			if stopWhenEmpty {
				logger.Printf("drained all pending log batches")
			}
			return
		}

		now := time.Now()
		if !stopWhenEmpty && !f.lastFlushAt.IsZero() && now.Sub(f.lastFlushAt) < f.flushInterval {
			return
		}

		batchPath := batchPaths[0]
		batch, err := f.spoolQueue.readBatch(batchPath)
		if err != nil {
			logger.Printf("ERROR failed to read pending log batch %s: %v", batchPath, err)
			time.Sleep(backoff)
			backoff = minDuration(backoff*2, f.retryMax)
			return
		}

		if err := f.putBatch(batch); err != nil {
			logger.Printf("ERROR failed to push %d log lines to OCI Logging: %v", len(batch.Lines), err)
			time.Sleep(backoff)
			backoff = minDuration(backoff*2, f.retryMax)
			return
		}

		if err := os.Remove(batchPath); err != nil {
			logger.Printf("ERROR failed to delete flushed batch %s: %v", batchPath, err)
			time.Sleep(backoff)
			backoff = minDuration(backoff*2, f.retryMax)
			return
		}

		f.lastFlushAt = time.Now()
		backoff = f.retryInitial
	}
}

// putBatch converts one read batch into the OCI Logging ingestion payload.
func (f *ociLogForwarder) putBatch(batch readBatch) error {
	timestamp := common.SDKTime{Time: time.Now().UTC()}
	entries := make([]loggingingestion.LogEntry, 0, len(batch.Lines))
	for _, line := range batch.Lines {
		id := randomRequestID()
		lineCopy := line
		entries = append(entries, loggingingestion.LogEntry{
			Data: common.String(lineCopy),
			Id:   common.String(id),
			Time: &timestamp,
		})
	}

	logSource := f.logSource
	logType := f.logType
	logSubject := f.logSubject
	putLogsDetails := loggingingestion.PutLogsDetails{
		Specversion: common.String("1.0"),
		LogEntryBatches: []loggingingestion.LogEntryBatch{
			{
				Entries:             entries,
				Source:              common.String(logSource),
				Type:                common.String(logType),
				Subject:             common.String(logSubject),
				Defaultlogentrytime: &timestamp,
			},
		},
	}

	request := loggingingestion.PutLogsRequest{
		LogId:          common.String(f.logID),
		PutLogsDetails: putLogsDetails,
	}
	_, err := f.client.PutLogs(context.Background(), request)
	if err != nil {
		return err
	}
	logger.Printf("pushed %d log lines to OCI Logging", len(batch.Lines))
	return nil
}

// buildLoggingClient creates an OCI Logging ingestion client that only uses the
// resource principal supplied by the runtime environment.
func buildLoggingClient() (loggingingestion.LoggingClient, error) {
	provider, err := auth.ResourcePrincipalConfigurationProvider()
	if err != nil {
		return loggingingestion.LoggingClient{}, err
	}
	client, err := loggingingestion.NewLoggingClientWithConfigurationProvider(provider)
	if err != nil {
		return loggingingestion.LoggingClient{}, err
	}

	region := resolveRegion(provider)
	if region != "" {
		client.SetRegion(region)
	}
	return client, nil
}

func resolveRegion(provider common.ConfigurationProvider) string {
	if explicit := strings.TrimSpace(os.Getenv("OCI_REGION")); explicit != "" {
		return explicit
	}
	for _, name := range []string{
		"OCI_RESOURCE_PRINCIPAL_REGION",
		"OCI_RESOURCE_PRINCIPAL_REGION_FOR_LEAF_RESOURCE",
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	if provider == nil {
		return ""
	}
	region, err := provider.Region()
	if err != nil {
		return ""
	}
	return region
}

func getenvRequired(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		panic(fmt.Sprintf("missing required environment variable: %s", name))
	}
	return value
}

func getenvDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseDuration(value string) time.Duration {
	raw := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.HasSuffix(raw, "ms"),
		strings.HasSuffix(raw, "s"),
		strings.HasSuffix(raw, "m"),
		strings.HasSuffix(raw, "h"):
		duration, err := time.ParseDuration(raw)
		if err != nil {
			panic(err)
		}
		return duration
	default:
		seconds, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			panic(err)
		}
		return time.Duration(seconds * float64(time.Second))
	}
}

func parseSize(value string) int {
	raw := strings.ToLower(strings.TrimSpace(value))
	multipliers := map[string]int64{
		"k":  1024,
		"kb": 1024,
		"m":  1024 * 1024,
		"mb": 1024 * 1024,
		"g":  1024 * 1024 * 1024,
		"gb": 1024 * 1024 * 1024,
	}
	for suffix, multiplier := range multipliers {
		if strings.HasSuffix(raw, suffix) {
			valuePart := strings.TrimSpace(strings.TrimSuffix(raw, suffix))
			number, err := strconv.ParseFloat(valuePart, 64)
			if err != nil {
				panic(err)
			}
			return int(number * float64(multiplier))
		}
	}
	number, err := strconv.Atoi(raw)
	if err != nil {
		panic(err)
	}
	return number
}

func formatSizeBytes(sizeBytes int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(sizeBytes)
	unit := units[0]
	for _, candidate := range units {
		unit = candidate
		if size < 1024 || candidate == units[len(units)-1] {
			break
		}
		size /= 1024
	}
	if unit == "B" {
		return fmt.Sprintf("%d %s", int(size), unit)
	}
	return fmt.Sprintf("%.1f %s", size, unit)
}

func mustAtoi(value string) int {
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		panic(err)
	}
	return number
}

func hostName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "unknown"
	}
	return name
}

func randomRequestID() string {
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(randomBytes)
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	exitCode := 0
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Printf("ERROR log forwarder failed: %v", recovered)
				exitCode = 1
			}
		}()

		logForwarder, err := newOciLogForwarder()
		if err != nil {
			logger.Printf("ERROR log forwarder failed: %v", err)
			exitCode = 1
			return
		}
		exitCode = logForwarder.start(ctx)
	}()

	os.Exit(exitCode)
}
