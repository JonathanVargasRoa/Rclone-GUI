---
title: "OpenDrive"
description: "Rclone docs for OpenDrive"
date: "2017-08-07"
---

<i class="fa fa-file"></i> OpenDrive
------------------------------------

Paths are specified as `remote:path`

Paths may be as deep as required, eg `remote:directory/subdirectory`.

Here is an example of how to make a remote called `remote`.  First run:

     rclone config

This will guide you through an interactive setup process:

```
n) New remote
d) Delete remote
q) Quit config
e/n/d/q> n
name> remote
Type of storage to configure.
Choose a number from below, or type in your own value
 1 / Amazon Drive
   \ "amazon cloud drive"
 2 / Amazon S3 (also Dreamhost, Ceph, Minio)
   \ "s3"
 3 / Backblaze B2
   \ "b2"
 4 / Dropbox
   \ "dropbox"
 5 / Encrypt/Decrypt a remote
   \ "crypt"
 6 / Google Cloud Storage (this is not Google Drive)
   \ "google cloud storage"
 7 / Google Drive
   \ "drive"
 8 / Hubic
   \ "hubic"
 9 / Local Disk
   \ "local"
10 / OpenDrive
   \ "opendrive"
11 / Microsoft OneDrive
   \ "onedrive"
12 / Openstack Swift (Rackspace Cloud Files, Memset Memstore, OVH)
   \ "swift"
13 / SSH/SFTP Connection
   \ "sftp"
14 / Yandex Disk
   \ "yandex"
Storage> 10
Username
username>
Password
y) Yes type in my own password
g) Generate random password
y/g> y
Enter the password:
password:
Confirm the password:
password:
--------------------
[remote]
username =
password = *** ENCRYPTED ***
--------------------
y) Yes this is OK
e) Edit this remote
d) Delete this remote
y/e/d> y
```

List directories in top level of your OpenDrive

    rclone lsd remote:

List all the files in your OpenDrive

    rclone ls remote:

To copy a local directory to an OpenDrive directory called backup

    rclone copy /home/source remote:backup

### Modified time and MD5SUMs ###

OpenDrive allows modification times to be set on objects accurate to 1
second. These will be used to detect whether objects need syncing or
not.

### Deleting files ###

Any files you delete with rclone will end up in the trash. Amazon
don't provide an API to permanently delete files, nor to empty the
trash, so you will have to do that with one of Amazon's apps or via
the OpenDrive website. As of November 17, 2016, files are 
automatically deleted by Amazon from the trash after 30 days.

### Limitations ###

Note that OpenDrive is case insensitive so you can't have a
file called "Hello.doc" and one called "hello.doc".

There are quite a few characters that can't be in OpenDrive file
names.  These can't occur on Windows platforms, but on non-Windows
platforms they are common.  Rclone will map these names to and from an
identical looking unicode equivalent.  For example if a file has a `?`
in it will be mapped to `???` instead.

