# Container Instance Hello World

The goal of this tutorial is to launch a Container Instance using a public instance and be able to see webpage served by NGINX through your browser.

As you make your way through this tutorial, look out for this icon ![user input icon](../../images/userinput.png).
Whenever you see it, it's time for you to perform an action.

## Prerequisites

* [Getting Started](../../GETTINGSTARTED.md)

## Setup Network Security Policy
![user input icon](../../images/userinput.png)

Your network should already be setup with a public subnet.  You need to add the following to allow the instance to be publicly accessed

1. Sign in to the OCI Console as the new user created with permissions to use Container Instances and modify Networking.
2. In the Console, open the navigation menu and click Networking. Then select Virtual Cloud Networks.
3. Select the VCN you created during Getting Started for the purposes of trying out Container Instances
4. Under **Resources** to the left, select **Security Lists**
5. Click the Default Security List policy that is listed.
6. Click **Add Ingress Rules**
7. Fill out the following parameters in the prompt.
Stateless: No
Source Cider: 0.0.0.0/0
TCP: All
Destination Port Range: 80
8. Click Add Ingress Rules

Now you should be able to access your VCN through public ip addresses assigned to your Container Instances.

Note, this vcn should be for demo purposes only and should not contain any production instances.  

## Create Container Instance
![user input icon](../..//images/userinput.png)

Follow [Getting Started](../../GETTINGSTARTED.md) instructions for launching an instance.  
However, for **Registry Hostname** use "docker.io" **Repository** please use "nginxdemos/hello" instead. 

## Access Instance  
![user input icon](../../images/userinput.png)

1. Gather Public IP Address for Container Instance page
2. In browser, type <public ip adress>:80
3. View the NGINX page served for your Container Instance

Congratulations! You've just created a Container Instance with OCI and were able to view its public web page!

## Delete Network Security Policy
![user input icon](../../images/userinput.png)

For security reasons, it may make sense to remove the additional security list policy you added to allow internet traffic to your VCN