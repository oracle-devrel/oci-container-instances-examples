# Container Instance Hello World

The goal of this tutorial is to launch a Container Instance using a public instance and be able to see webpage served by NGINX.

As you make your way through this tutorial, look out for this icon ![user input icon](../../images/userinput.png).
Whenever you see it, it's time for you to perform an action.

## Prerequisites

* [Getting Started](../../GETTINGSTARTED.md)

## Setup Network Security Policy
![user input icon](../../images/userinput.png)

Your network should already be setup with a public subnet.  You need to add the following to allow the instance to be queried to publicly

Stateless: No
Source Cider: 0.0.0.0/0
TCP: All
Destination Port Range: 80

Note, this vcn should be for demo purposes only and should not contain any production instances.  If you choose to do this, you may consider deleting modificaiton to security list

## Launch Instance
![user input icon](../..//images/userinput.png)

repo: docker.io/
image: nginxdemos/hello

## Access Instance  
![user input icon](../../images/userinput.png)

1. Gather Public IP Adress for Container Instance page
2. In browser, type <public ip adress>:80
3. View the NGINX page served for your Container Instance

Congratulations! You've just created your first Container Instance with OCI and were able to view its web page!

## Delete Network Security Policy
![user input icon](../../images/userinput.png)

For security reasons, it may make sense to remove the additional security list policy you added to allow internet traffic to your VCN