# Getting Started with Oracle Container Instances

### A. Set up your tenancy

If suitable users and groups don't exist already:
1. Sign into the Console as a tenancy administrator.
2. Open the navigation menu and click Identity & Security. Under Identity, click Groups.
3. Create a new group by clicking Groups and then Create Group.
4. Create a new user by clicking **Users** and then **Create User**.

If a suitable compartment in which to create network resources and OCI Container Instances resources doesn't exist already:

1. Sign in to the Console as a tenancy administrator.
2. Open the navigation menu and click Identity & Security. Under Identity, click Compartments.
3. Click Create Compartment.

If a suitable VCN in which to create network resources doesn't exist already:

1. Sign in to the Console as a tenancy administrator.
2. Open the navigation menu, click Networking, and then click Virtual cloud networks.
3. Click Start VCN Wizard to create a new VCN.
4. In the Start VCN Wizard dialog box, select VCN with Internet Connectivity and click Start VCN Wizard.
5. Enter a name for the new VCN, click Next, and then click Create to create the VCN along with the related network resources.

Note that the networking configuration for your real application should be secure per your requirements.  These instructions are meant for demo purposes where you pull images from a public repo.

If one or more OCI Container Instance users is not a tenancy administrator:

1. Sign in to the Console as a tenancy administrator.
2. Open the navigation menu and click Identity & Security. Under Identity, click Policies.
3. Click Create Policy, specify a name and description for the new policy, and select the tenancy's root compartment.
4. Use the Policy Builder to create the policy. Select Functions from the list of Policy use cases, and base the policy on the policy template Let users create, deploy, and manage functions and applications.

The policy template includes the following policy statements:


If necessary, you can restrict these policy statements by compartment.

