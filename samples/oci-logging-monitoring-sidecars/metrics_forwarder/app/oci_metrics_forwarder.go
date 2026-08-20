// OCI metrics forwarder for the sidecar sample.
//
// The forwarder tails a shared JSON-lines metrics file, persists offsets by
// inode so rotation can be drained safely, stages batches on disk, validates
// each metric record, and ships the results to OCI Monitoring.
package main

import (
	"bufio"
	"context"
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

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/monitoring"
)

var logger = log.New(os.Stdout, "[metrics-forwarder] ", log.LstdFlags)

// trackedFile stores the current read position for one inode-backed file.
type trackedFile struct {
	Path   string `json:"path"`
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

type fileReadBatch struct {
	SourcePath string   `json:"source_path"`
	Inode      uint64   `json:"inode"`
	EndOffset  int64    `json:"end_offset"`
	Lines      []string `json:"lines"`
}

type metricRecord struct {
	Name          string            `json:"name"`
	Value         float64           `json:"value"`
	Timestamp     string            `json:"timestamp"`
	Dimensions    map[string]string `json:"dimensions"`
	ResourceGroup *string           `json:"resource_group"`
	Metadata      map[string]string `json:"metadata"`
	Namespace     string            `json:"namespace"`
	CompartmentID string            `json:"compartment_id"`
}

type metricBatch struct {
	SourcePath string         `json:"source_path"`
	Inode      uint64         `json:"inode"`
	EndOffset  int64          `json:"end_offset"`
	Records    []metricRecord `json:"records"`
}

type statePayload struct {
	TrackedFiles []trackedFile `json:"tracked_files"`
}

// spoolQueue persists pending metric batches before OCI API calls succeed.
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
		payload, err := q.readBatch(batchPath)
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

func (q *spoolQueue) writeBatch(batch metricBatch) error {
	data, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	batchName := fmt.Sprintf("%020d.json", time.Now().UnixNano())
	tmpPath := filepath.Join(q.spoolDir, "."+batchName+".tmp")
	finalPath := filepath.Join(q.spoolDir, batchName)
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

func (q *spoolQueue) readBatch(path string) (metricBatch, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return metricBatch{}, err
	}
	var payload metricBatch
	if err := json.Unmarshal(data, &payload); err != nil {
		return metricBatch{}, err
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

func (t *fileTracker) ensureMetricFile() error {
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
	if err := t.ensureMetricFile(); err != nil {
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
	if err := t.ensureMetricFile(); err != nil {
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
			logger.Printf("WARNING current file inode=%d moved but no rotated file was found; unread metric data may be lost", t.trackedFiles[currentIndex].Inode)
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
		logger.Printf("WARNING current metric file %s shrank from %d bytes to %d bytes; rewinding tracked offset", t.path, t.trackedFiles[currentIndex].Offset, currentSize)
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
			logger.Printf("WARNING tracked rotated metric file inode=%d disappeared before it was fully consumed", tracked.Inode)
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

func (t *fileTracker) readBatch(maxLines int, maxBytes int) (*fileReadBatch, error) {
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
				logger.Printf("WARNING tracked metric file inode=%d disappeared before it could be consumed", tracked.Inode)
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
			return &fileReadBatch{
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

func (t *fileTracker) markConsumed(sourcePath string, inode uint64, endOffset int64) error {
	index := -1
	for i, item := range t.trackedFiles {
		if item.Inode == inode && item.Path == sourcePath {
			index = i
			break
		}
	}
	if index < 0 {
		for i, item := range t.trackedFiles {
			if item.Inode == inode {
				index = i
				break
			}
		}
	}
	if index < 0 {
		t.trackedFiles = append(t.trackedFiles, trackedFile{Path: sourcePath, Inode: inode, Offset: endOffset})
	} else {
		t.trackedFiles[index].Path = sourcePath
		if t.trackedFiles[index].Offset < endOffset {
			t.trackedFiles[index].Offset = endOffset
		}
	}
	t.reorderTrackedFiles(t.trackedFiles)
	return t.persistState()
}

type ociMetricsForwarder struct {
	client               monitoring.MonitoringClient
	metricNamespace      string
	compartmentID        string
	resourceGroup        *string
	flushInterval        time.Duration
	chunkLimitBytes      int
	maxQueuedBatches     int
	maxBatchEntries      int
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

// newOciMetricsForwarder resolves environment variables and reconstructs any
// durable state that was left behind by earlier runs.
func newOciMetricsForwarder() (*ociMetricsForwarder, error) {
	client, err := buildMonitoringClient()
	if err != nil {
		return nil, err
	}

	metricFilePath := getenvRequired("METRIC_FILE_PATH")
	stateDir := getenvDefault("METRICS_FORWARDER_STATE_DIR", "/var/lib/oci-metrics-forwarder/state")
	spoolDir := getenvDefault("METRICS_FORWARDER_SPOOL_DIR", "/var/lib/oci-metrics-forwarder/spool")
	statePath := getenvDefault("METRIC_STATE_FILE", filepath.Join(stateDir, "input.json"))
	queueDir := getenvDefault("METRIC_QUEUE_DIR", spoolDir)

	spoolQueue, err := newSpoolQueue(queueDir)
	if err != nil {
		return nil, err
	}
	recoveredOffsets := spoolQueue.recoverOffsets()
	fileTracker, err := newFileTracker(metricFilePath, statePath, parseBool(getenvDefault("READ_FROM_HEAD", "true")), recoveredOffsets)
	if err != nil {
		return nil, err
	}

	resourceGroup := optionalString(os.Getenv("OCI_MONITORING_RESOURCE_GROUP"))
	return &ociMetricsForwarder{
		client:               client,
		metricNamespace:      getenvRequired("OCI_MONITORING_NAMESPACE"),
		compartmentID:        getenvRequired("OCI_MONITORING_COMPARTMENT_ID"),
		resourceGroup:        resourceGroup,
		flushInterval:        parseDuration(getenvDefault("METRICS_FORWARDER_FLUSH_INTERVAL", "5s")),
		chunkLimitBytes:      parseSize(getenvDefault("METRICS_FORWARDER_CHUNK_LIMIT_SIZE", "1m")),
		maxQueuedBatches:     mustAtoi(getenvDefault("METRICS_FORWARDER_QUEUED_BATCH_LIMIT", "64")),
		maxBatchEntries:      mustAtoi(getenvDefault("OCI_MAX_BATCH_ENTRIES", "50")),
		pollInterval:         parseDuration(getenvDefault("METRIC_POLL_INTERVAL_SECONDS", "1")),
		diskUsageLogInterval: parseDuration(getenvDefault("METRICS_FORWARDER_DISK_USAGE_LOG_INTERVAL", "5m")),
		retryInitial:         parseDuration(getenvDefault("OCI_RETRY_INITIAL_SECONDS", "1")),
		retryMax:             parseDuration(getenvDefault("OCI_RETRY_MAX_SECONDS", "30")),
		spoolQueue:           spoolQueue,
		fileTracker:          fileTracker,
	}, nil
}

// start runs the main poll, validate, spool, and flush loop until shutdown.
func (f *ociMetricsForwarder) start(ctx context.Context) int {
	logger.Printf("starting OCI metrics forwarder")
	logger.Printf("source file: %s", f.fileTracker.path)
	logger.Printf("OCI auth mode: resource_principal")
	logger.Printf("OCI Monitoring namespace: %s", f.metricNamespace)
	logger.Printf("OCI Monitoring compartment id: %s", f.compartmentID)
	f.logMetricFileUsageIfDue(true)

	for !f.stopRequested {
		select {
		case <-ctx.Done():
			logger.Printf("received signal; draining metric spool before exit")
			f.stopRequested = true
			continue
		default:
		}

		f.logMetricFileUsageIfDue(false)
		f.flushSpool(false)

		if f.spoolQueue.count() < f.maxQueuedBatches {
			fileBatch, err := f.fileTracker.readBatch(f.maxBatchEntries, f.chunkLimitBytes)
			if err != nil {
				logger.Printf("ERROR metrics forwarder failed: %v", err)
				return 1
			}
			if fileBatch != nil {
				metricBatch := f.buildMetricBatch(*fileBatch)
				if err := f.fileTracker.markConsumed(fileBatch.SourcePath, fileBatch.Inode, fileBatch.EndOffset); err != nil {
					logger.Printf("ERROR metrics forwarder failed: %v", err)
					return 1
				}
				if metricBatch == nil {
					continue
				}
				if err := f.spoolQueue.writeBatch(*metricBatch); err != nil {
					logger.Printf("ERROR metrics forwarder failed: %v", err)
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

// buildMetricBatch drops invalid records but keeps the valid subset of a file
// batch so one malformed line does not block the pipeline.
func (f *ociMetricsForwarder) buildMetricBatch(fileBatch fileReadBatch) *metricBatch {
	records := make([]metricRecord, 0, len(fileBatch.Lines))
	for _, line := range fileBatch.Lines {
		record := f.parseMetricLine(line)
		if record == nil {
			continue
		}
		records = append(records, *record)
	}
	if len(records) == 0 {
		logger.Printf("WARNING skipping fully invalid metric batch from %s", fileBatch.SourcePath)
		return nil
	}
	return &metricBatch{
		SourcePath: fileBatch.SourcePath,
		Inode:      fileBatch.Inode,
		EndOffset:  fileBatch.EndOffset,
		Records:    records,
	}
}

// parseMetricLine normalizes one JSON line into the structure expected by the
// OCI Monitoring SDK request payload.
func (f *ociMetricsForwarder) parseMetricLine(line string) *metricRecord {
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		logger.Printf("WARNING dropping invalid metric line %q: %v", line, err)
		return nil
	}

	name := strings.TrimSpace(fmt.Sprintf("%v", payload["name"]))
	if name == "" || name == "<nil>" {
		logger.Printf("WARNING dropping invalid metric line %q: metric name must not be empty", line)
		return nil
	}

	value, err := asFloat(payload["value"])
	if err != nil {
		logger.Printf("WARNING dropping invalid metric line %q: %v", line, err)
		return nil
	}

	timestamp, err := parseMetricTimestamp(payload["timestamp"])
	if err != nil {
		logger.Printf("WARNING dropping invalid metric line %q: %v", line, err)
		return nil
	}

	dimensions, err := asStringMap(payload["dimensions"])
	if err != nil {
		logger.Printf("WARNING dropping invalid metric line %q: %v", line, err)
		return nil
	}
	metadata, err := asStringMap(payload["metadata"])
	if err != nil {
		logger.Printf("WARNING dropping invalid metric line %q: %v", line, err)
		return nil
	}

	resourceGroup := f.resourceGroup
	if value, ok := payload["resource_group"]; ok {
		resourceGroup = optionalString(fmt.Sprintf("%v", value))
	}

	namespace := f.metricNamespace
	if value, ok := payload["namespace"]; ok {
		trimmed := strings.TrimSpace(fmt.Sprintf("%v", value))
		if trimmed != "" && trimmed != "<nil>" {
			namespace = trimmed
		}
	}

	compartmentID := f.compartmentID
	if value, ok := payload["compartment_id"]; ok {
		trimmed := strings.TrimSpace(fmt.Sprintf("%v", value))
		if trimmed != "" && trimmed != "<nil>" {
			compartmentID = trimmed
		}
	}

	return &metricRecord{
		Name:          name,
		Value:         value,
		Timestamp:     timestamp.Format(time.RFC3339Nano),
		Dimensions:    dimensions,
		ResourceGroup: resourceGroup,
		Metadata:      metadata,
		Namespace:     namespace,
		CompartmentID: compartmentID,
	}
}

func (f *ociMetricsForwarder) logMetricFileUsageIfDue(force bool) {
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
		"metric files consume %d (%s) across %d file(s) under %s",
		totalBytes,
		formatSizeBytes(totalBytes),
		fileCount,
		filepath.Dir(f.fileTracker.path),
	)
	f.nextDiskUsageLogAt = now.Add(f.diskUsageLogInterval)
}

// flushSpool retries queued batches with exponential backoff and keeps the
// oldest batch first to preserve ordering as much as possible.
func (f *ociMetricsForwarder) flushSpool(stopWhenEmpty bool) {
	backoff := f.retryInitial
	for {
		batchPaths := f.spoolQueue.listBatches()
		if len(batchPaths) == 0 {
			if stopWhenEmpty {
				logger.Printf("drained all pending metric batches")
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
			logger.Printf("ERROR failed to read pending metric batch %s: %v", batchPath, err)
			time.Sleep(backoff)
			backoff = minDuration(backoff*2, f.retryMax)
			return
		}

		if err := f.putBatch(batch); err != nil {
			logger.Printf("ERROR failed to push %d metric(s) to OCI Monitoring: %v", len(batch.Records), err)
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

// putBatch converts one validated metric batch into an OCI Monitoring request.
func (f *ociMetricsForwarder) putBatch(batch metricBatch) error {
	metricData := make([]monitoring.MetricDataDetails, 0, len(batch.Records))
	for _, record := range batch.Records {
		timestamp, err := parseMetricTimestamp(record.Timestamp)
		if err != nil {
			return err
		}
		value := record.Value
		datapoint := monitoring.Datapoint{
			Timestamp: &common.SDKTime{Time: timestamp},
			Value:     &value,
		}
		item := monitoring.MetricDataDetails{
			Namespace:     common.String(record.Namespace),
			CompartmentId: common.String(record.CompartmentID),
			Name:          common.String(record.Name),
			Dimensions:    record.Dimensions,
			Metadata:      record.Metadata,
			Datapoints:    []monitoring.Datapoint{datapoint},
			ResourceGroup: record.ResourceGroup,
		}
		metricData = append(metricData, item)
	}

	request := monitoring.PostMetricDataRequest{
		PostMetricDataDetails: monitoring.PostMetricDataDetails{
			MetricData: metricData,
		},
	}
	_, err := f.client.PostMetricData(context.Background(), request)
	if err != nil {
		return err
	}
	logger.Printf("pushed %d metric(s) to OCI Monitoring", len(batch.Records))
	return nil
}

// buildMonitoringClient creates an OCI Monitoring client that only uses the
// resource principal supplied by the runtime environment.
func buildMonitoringClient() (monitoring.MonitoringClient, error) {
	provider, err := auth.ResourcePrincipalConfigurationProvider()
	if err != nil {
		return monitoring.MonitoringClient{}, err
	}
	client, err := monitoring.NewMonitoringClientWithConfigurationProvider(provider)
	if err != nil {
		return monitoring.MonitoringClient{}, err
	}

	region := resolveRegion(provider)
	endpoint, err := resolveMonitoringIngestionEndpoint(client, region)
	if err != nil {
		return monitoring.MonitoringClient{}, err
	}
	client.Host = endpoint
	logger.Printf("using OCI Monitoring telemetry ingestion endpoint %s", endpoint)
	return client, nil
}

func resolveMonitoringIngestionEndpoint(client monitoring.MonitoringClient, region string) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("OCI_MONITORING_INGESTION_ENDPOINT")); explicit != "" {
		return explicit, nil
	}
	currentEndpoint := strings.TrimSpace(client.Host)
	if currentEndpoint != "" {
		if strings.Contains(currentEndpoint, "://telemetry-ingestion.") {
			return currentEndpoint, nil
		}
		if strings.Contains(currentEndpoint, "://telemetry.") {
			return strings.Replace(currentEndpoint, "://telemetry.", "://telemetry-ingestion.", 1), nil
		}
	}
	if region != "" {
		return fmt.Sprintf("https://telemetry-ingestion.%s.oraclecloud.com", region), nil
	}
	return "", fmt.Errorf("unable to determine OCI Monitoring telemetry ingestion endpoint; set OCI_REGION or OCI_MONITORING_INGESTION_ENDPOINT")
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

func parseMetricTimestamp(value any) (time.Time, error) {
	if value == nil {
		return time.Now().UTC(), nil
	}
	switch typed := value.(type) {
	case float64:
		return time.Unix(0, int64(typed*float64(time.Second))).UTC(), nil
	case int64:
		return time.Unix(typed, 0).UTC(), nil
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return time.Time{}, err
		}
		return time.Unix(0, int64(number*float64(time.Second))).UTC(), nil
	case string:
		normalized := strings.TrimSpace(typed)
		if normalized == "" {
			return time.Now().UTC(), nil
		}
		normalized = strings.ReplaceAll(normalized, "Z", "+00:00")
		parsed, err := time.Parse(time.RFC3339Nano, normalized)
		if err != nil {
			return time.Time{}, err
		}
		return parsed.UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp value: %v", value)
	}
}

func asFloat(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case float32:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case json.Number:
		return typed.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(typed), 64)
	default:
		return 0, fmt.Errorf("unsupported metric value: %v", value)
	}
}

func asStringMap(value any) (map[string]string, error) {
	if value == nil {
		return map[string]string{}, nil
	}
	typed, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("value must be a JSON object")
	}
	result := make(map[string]string, len(typed))
	for key, raw := range typed {
		result[fmt.Sprintf("%v", key)] = fmt.Sprintf("%v", raw)
	}
	return result, nil
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

func optionalString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
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
				logger.Printf("ERROR metrics forwarder failed: %v", recovered)
				exitCode = 1
			}
		}()

		forwarder, err := newOciMetricsForwarder()
		if err != nil {
			logger.Printf("ERROR metrics forwarder failed: %v", err)
			exitCode = 1
			return
		}
		exitCode = forwarder.start(ctx)
	}()

	os.Exit(exitCode)
}
