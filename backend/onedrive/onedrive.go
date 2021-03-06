// Package onedrive provides an interface to the Microsoft OneDrive
// object storage system.
package onedrive

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/ncw/rclone/backend/onedrive/api"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/config"
	"github.com/ncw/rclone/fs/config/configmap"
	"github.com/ncw/rclone/fs/config/configstruct"
	"github.com/ncw/rclone/fs/config/obscure"
	"github.com/ncw/rclone/fs/fserrors"
	"github.com/ncw/rclone/fs/hash"
	"github.com/ncw/rclone/lib/dircache"
	"github.com/ncw/rclone/lib/oauthutil"
	"github.com/ncw/rclone/lib/pacer"
	"github.com/ncw/rclone/lib/readers"
	"github.com/ncw/rclone/lib/rest"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"
)

const (
	rcloneClientID              = "b15665d9-eda6-4092-8539-0eec376afd59"
	rcloneEncryptedClientSecret = "_JUdzh3LnKNqSPcf4Wu5fgMFIQOI8glZu_akYgR8yf6egowNBg-R"
	minSleep                    = 10 * time.Millisecond
	maxSleep                    = 2 * time.Second
	decayConstant               = 2 // bigger for slower decay, exponential
	graphURL                    = "https://graph.microsoft.com/v1.0"
	configDriveID               = "drive_id"
	configDriveType             = "drive_type"
	driveTypePersonal           = "personal"
	driveTypeBusiness           = "business"
	driveTypeSharepoint         = "documentLibrary"
)

// Globals
var (
	// Description of how to auth for this app for a business account
	oauthConfig = &oauth2.Config{
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		},
		Scopes:       []string{"Files.Read", "Files.ReadWrite", "Files.Read.All", "Files.ReadWrite.All", "offline_access"},
		ClientID:     rcloneClientID,
		ClientSecret: obscure.MustReveal(rcloneEncryptedClientSecret),
		RedirectURL:  oauthutil.RedirectLocalhostURL,
	}
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "onedrive",
		Description: "Microsoft OneDrive",
		NewFs:       NewFs,
		Config: func(name string, m configmap.Mapper) {
			err := oauthutil.Config("onedrive", name, m, oauthConfig)
			if err != nil {
				log.Fatalf("Failed to configure token: %v", err)
				return
			}

			// Are we running headless?
			if automatic, _ := m.Get(config.ConfigAutomatic); automatic != "" {
				// Yes, okay we are done
				return
			}

			type driveResource struct {
				DriveID   string `json:"id"`
				DriveName string `json:"name"`
				DriveType string `json:"driveType"`
			}
			type drivesResponse struct {
				Drives []driveResource `json:"value"`
			}

			type siteResource struct {
				SiteID   string `json:"id"`
				SiteName string `json:"displayName"`
				SiteURL  string `json:"webUrl"`
			}
			type siteResponse struct {
				Sites []siteResource `json:"value"`
			}

			oAuthClient, _, err := oauthutil.NewClient(name, m, oauthConfig)
			if err != nil {
				log.Fatalf("Failed to configure OneDrive: %v", err)
			}
			srv := rest.NewClient(oAuthClient)

			var opts rest.Opts
			var finalDriveID string
			var siteID string
			switch config.Choose("Your choice",
				[]string{"onedrive", "sharepoint", "driveid", "siteid", "search"},
				[]string{"OneDrive Personal or Business", "Sharepoint site", "Type in driveID", "Type in SiteID", "Search a Sharepoint site"},
				false) {

			case "onedrive":
				opts = rest.Opts{
					Method:  "GET",
					RootURL: graphURL,
					Path:    "/me/drives",
				}
			case "sharepoint":
				opts = rest.Opts{
					Method:  "GET",
					RootURL: graphURL,
					Path:    "/sites/root/drives",
				}
			case "driveid":
				fmt.Printf("Paste your Drive ID here> ")
				finalDriveID = config.ReadLine()
			case "siteid":
				fmt.Printf("Paste your Site ID here> ")
				siteID = config.ReadLine()
			case "search":
				fmt.Printf("What to search for> ")
				searchTerm := config.ReadLine()
				opts = rest.Opts{
					Method:  "GET",
					RootURL: graphURL,
					Path:    "/sites?search=" + searchTerm,
				}

				sites := siteResponse{}
				_, err := srv.CallJSON(&opts, nil, &sites)
				if err != nil {
					log.Fatalf("Failed to query available sites: %v", err)
				}

				if len(sites.Sites) == 0 {
					log.Fatalf("Search for '%s' returned no results", searchTerm)
				} else {
					fmt.Printf("Found %d sites, please select the one you want to use:\n", len(sites.Sites))
					for index, site := range sites.Sites {
						fmt.Printf("%d: %s (%s) id=%s\n", index, site.SiteName, site.SiteURL, site.SiteID)
					}
					siteID = sites.Sites[config.ChooseNumber("Chose drive to use:", 0, len(sites.Sites)-1)].SiteID
				}
			}

			// if we have a siteID we need to ask for the drives
			if siteID != "" {
				opts = rest.Opts{
					Method:  "GET",
					RootURL: graphURL,
					Path:    "/sites/" + siteID + "/drives",
				}
			}

			// We don't have the final ID yet?
			// query Microsoft Graph
			if finalDriveID == "" {
				drives := drivesResponse{}
				_, err := srv.CallJSON(&opts, nil, &drives)
				if err != nil {
					log.Fatalf("Failed to query available drives: %v", err)
				}

				if len(drives.Drives) == 0 {
					log.Fatalf("No drives found")
				} else {
					fmt.Printf("Found %d drives, please select the one you want to use:\n", len(drives.Drives))
					for index, drive := range drives.Drives {
						fmt.Printf("%d: %s (%s) id=%s\n", index, drive.DriveName, drive.DriveType, drive.DriveID)
					}
					finalDriveID = drives.Drives[config.ChooseNumber("Chose drive to use:", 0, len(drives.Drives)-1)].DriveID
				}
			}

			// Test the driveID and get drive type
			opts = rest.Opts{
				Method:  "GET",
				RootURL: graphURL,
				Path:    "/drives/" + finalDriveID + "/root"}
			var rootItem api.Item
			_, err = srv.CallJSON(&opts, nil, &rootItem)
			if err != nil {
				log.Fatalf("Failed to query root for drive %s: %v", finalDriveID, err)
			}

			fmt.Printf("Found drive '%s' of type '%s', URL: %s\nIs that okay?\n", rootItem.Name, rootItem.ParentReference.DriveType, rootItem.WebURL)
			// This does not work, YET :)
			if !config.Confirm() {
				log.Fatalf("Cancelled by user")
			}

			config.FileSet(name, configDriveID, finalDriveID)
			config.FileSet(name, configDriveType, rootItem.ParentReference.DriveType)
		},
		Options: []fs.Option{{
			Name: config.ConfigClientID,
			Help: "Microsoft App Client Id\nLeave blank normally.",
		}, {
			Name: config.ConfigClientSecret,
			Help: "Microsoft App Client Secret\nLeave blank normally.",
		}, {
			Name:     "chunk_size",
			Help:     "Chunk size to upload files with - must be multiple of 320k.",
			Default:  fs.SizeSuffix(10 * 1024 * 1024),
			Advanced: true,
		}, {
			Name:     "drive_id",
			Help:     "The ID of the drive to use",
			Default:  "",
			Advanced: true,
		}, {
			Name:     "drive_type",
			Help:     "The type of the drive ( personal | business | documentLibrary )",
			Default:  "",
			Advanced: true,
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	ChunkSize fs.SizeSuffix `config:"chunk_size"`
	DriveID   string        `config:"drive_id"`
	DriveType string        `config:"drive_type"`
}

// Fs represents a remote one drive
type Fs struct {
	name         string             // name of this remote
	root         string             // the path we are working on
	opt          Options            // parsed options
	features     *fs.Features       // optional features
	srv          *rest.Client       // the connection to the one drive server
	dirCache     *dircache.DirCache // Map of directory path to directory id
	pacer        *pacer.Pacer       // pacer for API calls
	tokenRenewer *oauthutil.Renew   // renew the token on expiry
	driveID      string             // ID to use for querying Microsoft Graph
	driveType    string             // https://developer.microsoft.com/en-us/graph/docs/api-reference/v1.0/resources/drive
}

// Object describes a one drive object
//
// Will definitely have info but maybe not meta
type Object struct {
	fs           *Fs       // what this object is part of
	remote       string    // The remote path
	hasMetaData  bool      // whether info below has been set
	size         int64     // size of the object
	modTime      time.Time // modification time of the object
	id           string    // ID of the object
	sha1         string    // SHA-1 of the object content
	quickxorhash string    // QuickXorHash of the object content
	mimeType     string    // Content-Type of object from server (may not be as uploaded)
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("One drive root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// parsePath parses an one drive 'url'
func parsePath(path string) (root string) {
	root = strings.Trim(path, "/")
	return
}

// retryErrorCodes is a slice of error codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

// shouldRetry returns a boolean as to whether this resp and err
// deserve to be retried.  It returns the err as a convenience
func shouldRetry(resp *http.Response, err error) (bool, error) {
	authRety := false

	if resp != nil && resp.StatusCode == 401 && len(resp.Header["Www-Authenticate"]) == 1 && strings.Index(resp.Header["Www-Authenticate"][0], "expired_token") >= 0 {
		authRety = true
		fs.Debugf(nil, "Should retry: %v", err)
	}
	return authRety || fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// readMetaDataForPath reads the metadata from the path
func (f *Fs) readMetaDataForPath(path string) (info *api.Item, resp *http.Response, err error) {
	var opts rest.Opts
	if len(path) == 0 {
		opts = rest.Opts{
			Method: "GET",
			Path:   "/root",
		}
	} else {
		opts = rest.Opts{
			Method: "GET",
			Path:   "/root:/" + rest.URLPathEscape(replaceReservedChars(path)),
		}
	}
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, nil, &info)
		return shouldRetry(resp, err)
	})

	return info, resp, err
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	errResponse := new(api.Error)
	err := rest.DecodeJSON(resp, &errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
	}
	if errResponse.ErrorInfo.Code == "" {
		errResponse.ErrorInfo.Code = resp.Status
	}
	return errResponse
}

// NewFs constructs an Fs from the path, container:path
func NewFs(name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}
	if opt.ChunkSize%(320*1024) != 0 {
		return nil, errors.Errorf("chunk size %d is not a multiple of 320k", opt.ChunkSize)
	}

	if opt.DriveID == "" || opt.DriveType == "" {
		log.Fatalf("Unable to get drive_id and drive_type. If you are upgrading from older versions of rclone, please run `rclone config` and re-configure this backend.")
	}

	root = parsePath(root)
	oAuthClient, ts, err := oauthutil.NewClient(name, m, oauthConfig)
	if err != nil {
		log.Fatalf("Failed to configure OneDrive: %v", err)
	}

	f := &Fs{
		name:      name,
		root:      root,
		opt:       *opt,
		driveID:   opt.DriveID,
		driveType: opt.DriveType,
		srv:       rest.NewClient(oAuthClient).SetRoot(graphURL + "/drives/" + opt.DriveID),
		pacer:     pacer.New().SetMinSleep(minSleep).SetMaxSleep(maxSleep).SetDecayConstant(decayConstant),
	}
	f.features = (&fs.Features{
		CaseInsensitive:         true,
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(f)
	f.srv.SetErrorHandler(errorHandler)

	// Renew the token in the background
	f.tokenRenewer = oauthutil.NewRenew(f.String(), ts, func() error {
		_, _, err := f.readMetaDataForPath("")
		return err
	})

	// Get rootID
	rootInfo, _, err := f.readMetaDataForPath("")
	if err != nil || rootInfo.ID == "" {
		return nil, errors.Wrap(err, "failed to get root")
	}

	f.dirCache = dircache.New(root, rootInfo.ID, f)

	// Find the current root
	err = f.dirCache.FindRoot(false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		newF := *f
		newF.dirCache = dircache.New(newRoot, rootInfo.ID, &newF)
		newF.root = newRoot
		// Make new Fs which is the parent
		err = newF.dirCache.FindRoot(false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := newF.newObjectWithInfo(remote, nil)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				// File doesn't exist so return old f
				return f, nil
			}
			return nil, err
		}
		// return an error with an fs which points to the parent
		return &newF, fs.ErrorIsFile
	}
	return f, nil
}

// rootSlash returns root with a slash on if it is empty, otherwise empty string
func (f *Fs) rootSlash() string {
	if f.root == "" {
		return f.root
	}
	return f.root + "/"
}

// Return an Object from a path
//
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) newObjectWithInfo(remote string, info *api.Item) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	var err error
	if info != nil {
		// Set info
		err = o.setMetaData(info)
	} else {
		err = o.readMetaData() // reads info and meta, returning an error
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(remote string) (fs.Object, error) {
	return f.newObjectWithInfo(remote, nil)
}

// FindLeaf finds a directory of name leaf in the folder with ID pathID
func (f *Fs) FindLeaf(pathID, leaf string) (pathIDOut string, found bool, err error) {
	// fs.Debugf(f, "FindLeaf(%q, %q)", pathID, leaf)
	parent, ok := f.dirCache.GetInv(pathID)
	if !ok {
		return "", false, errors.New("couldn't find parent ID")
	}
	path := leaf
	if parent != "" {
		path = parent + "/" + path
	}
	if f.dirCache.FoundRoot() {
		path = f.rootSlash() + path
	}
	info, resp, err := f.readMetaDataForPath(path)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	if info.GetFolder() == nil {
		return "", false, errors.New("found file when looking for folder")
	}
	return info.GetID(), true, nil
}

// CreateDir makes a directory with pathID as parent and name leaf
func (f *Fs) CreateDir(dirID, leaf string) (newID string, err error) {
	// fs.Debugf(f, "CreateDir(%q, %q)\n", dirID, leaf)
	var resp *http.Response
	var info *api.Item
	opts := newOptsCall(dirID, "POST", "/children")
	mkdir := api.CreateItemRequest{
		Name:             replaceReservedChars(leaf),
		ConflictBehavior: "fail",
	}
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, &mkdir, &info)
		return shouldRetry(resp, err)
	})
	if err != nil {
		//fmt.Printf("...Error %v\n", err)
		return "", err
	}

	//fmt.Printf("...Id %q\n", *info.Id)
	return info.GetID(), nil
}

// list the objects into the function supplied
//
// If directories is set it only sends directories
// User function to process a File item from listAll
//
// Should return true to finish processing
type listAllFn func(*api.Item) bool

// Lists the directory required calling the user function on each item found
//
// If the user fn ever returns true then it early exits with found = true
func (f *Fs) listAll(dirID string, directoriesOnly bool, filesOnly bool, fn listAllFn) (found bool, err error) {
	// Top parameter asks for bigger pages of data
	// https://dev.onedrive.com/odata/optional-query-parameters.htm
	opts := newOptsCall(dirID, "GET", "/children?$top=1000")
OUTER:
	for {
		var result api.ListChildrenResponse
		var resp *http.Response
		err = f.pacer.Call(func() (bool, error) {
			resp, err = f.srv.CallJSON(&opts, nil, &result)
			return shouldRetry(resp, err)
		})
		if err != nil {
			return found, errors.Wrap(err, "couldn't list files")
		}
		if len(result.Value) == 0 {
			break
		}
		for i := range result.Value {
			item := &result.Value[i]
			isFolder := item.GetFolder() != nil
			if isFolder {
				if filesOnly {
					continue
				}
			} else {
				if directoriesOnly {
					continue
				}
			}
			if item.Deleted != nil {
				continue
			}
			item.Name = restoreReservedChars(item.GetName())
			if fn(item) {
				found = true
				break OUTER
			}
		}
		if result.NextLink == "" {
			break
		}
		opts.Path = ""
		opts.RootURL = result.NextLink
	}
	return
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(dir string) (entries fs.DirEntries, err error) {
	err = f.dirCache.FindRoot(false)
	if err != nil {
		return nil, err
	}
	directoryID, err := f.dirCache.FindDir(dir, false)
	if err != nil {
		return nil, err
	}
	var iErr error
	_, err = f.listAll(directoryID, false, false, func(info *api.Item) bool {
		remote := path.Join(dir, info.GetName())
		folder := info.GetFolder()
		if folder != nil {
			// cache the directory ID for later lookups
			id := info.GetID()
			f.dirCache.Put(remote, id)
			d := fs.NewDir(remote, time.Time(info.GetLastModifiedDateTime())).SetID(id)
			if folder != nil {
				d.SetItems(folder.ChildCount)
			}
			entries = append(entries, d)
		} else {
			o, err := f.newObjectWithInfo(remote, info)
			if err != nil {
				iErr = err
				return true
			}
			entries = append(entries, o)
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if iErr != nil {
		return nil, iErr
	}
	return entries, nil
}

// Creates from the parameters passed in a half finished Object which
// must have setMetaData called on it
//
// Returns the object, leaf, directoryID and error
//
// Used to create new objects
func (f *Fs) createObject(remote string, modTime time.Time, size int64) (o *Object, leaf string, directoryID string, err error) {
	// Create the directory for the object if it doesn't exist
	leaf, directoryID, err = f.dirCache.FindRootAndPath(remote, true)
	if err != nil {
		return nil, leaf, directoryID, err
	}
	// Temporary Object under construction
	o = &Object{
		fs:     f,
		remote: remote,
	}
	return o, leaf, directoryID, nil
}

// Put the object into the container
//
// Copy the reader in to the new object which is returned
//
// The new object may have been created if an error is returned
func (f *Fs) Put(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()
	modTime := src.ModTime()

	o, _, _, err := f.createObject(remote, modTime, size)
	if err != nil {
		return nil, err
	}
	return o, o.Update(in, src, options...)
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(dir string) error {
	err := f.dirCache.FindRoot(true)
	if err != nil {
		return err
	}
	if dir != "" {
		_, err = f.dirCache.FindDir(dir, true)
	}
	return err
}

// deleteObject removes an object by ID
func (f *Fs) deleteObject(id string) error {
	opts := newOptsCall(id, "DELETE", "")
	opts.NoResponse = true

	return f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(&opts)
		return shouldRetry(resp, err)
	})
}

// purgeCheck removes the root directory, if check is set then it
// refuses to do so if it has anything in
func (f *Fs) purgeCheck(dir string, check bool) error {
	root := path.Join(f.root, dir)
	if root == "" {
		return errors.New("can't purge root directory")
	}
	dc := f.dirCache
	err := dc.FindRoot(false)
	if err != nil {
		return err
	}
	rootID, err := dc.FindDir(dir, false)
	if err != nil {
		return err
	}
	item, _, err := f.readMetaDataForPath(root)
	if err != nil {
		return err
	}
	if item.Folder == nil {
		return errors.New("not a folder")
	}
	if check && item.Folder.ChildCount != 0 {
		return errors.New("folder not empty")
	}
	err = f.deleteObject(rootID)
	if err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	if err != nil {
		return err
	}
	return nil
}

// Rmdir deletes the root folder
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(dir string) error {
	return f.purgeCheck(dir, true)
}

// Precision return the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// waitForJob waits for the job with status in url to complete
func (f *Fs) waitForJob(location string, o *Object) error {
	deadline := time.Now().Add(fs.Config.Timeout)
	for time.Now().Before(deadline) {
		var resp *http.Response
		var err error
		var body []byte
		err = f.pacer.Call(func() (bool, error) {
			resp, err = http.Get(location)
			if err != nil {
				return fserrors.ShouldRetry(err), err
			}
			body, err = rest.ReadBody(resp)
			return fserrors.ShouldRetry(err), err
		})
		if err != nil {
			return err
		}
		// Try to decode the body first as an api.AsyncOperationStatus
		var status api.AsyncOperationStatus
		err = json.Unmarshal(body, &status)
		if err != nil {
			return errors.Wrapf(err, "async status result not JSON: %q", body)
		}

		switch status.Status {
		case "failed":
		case "deleteFailed":
			{
				return errors.Errorf("%s: async operation returned %q", o.remote, status.Status)
			}
		case "completed":
			err = o.readMetaData()
			return errors.Wrapf(err, "async operation completed but readMetaData failed")
		}

		time.Sleep(1 * time.Second)
	}
	return errors.Errorf("async operation didn't complete after %v", fs.Config.Timeout)
}

// Copy src to this remote using server side copy operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	err := srcObj.readMetaData()
	if err != nil {
		return nil, err
	}

	srcPath := srcObj.fs.rootSlash() + srcObj.remote
	dstPath := f.rootSlash() + remote
	if strings.ToLower(srcPath) == strings.ToLower(dstPath) {
		return nil, errors.Errorf("can't copy %q -> %q as are same name when lowercase", srcPath, dstPath)
	}

	// Create temporary object
	dstObj, leaf, directoryID, err := f.createObject(remote, srcObj.modTime, srcObj.size)
	if err != nil {
		return nil, err
	}

	// Copy the object
	opts := newOptsCall(srcObj.id, "POST", "/copy")
	opts.ExtraHeaders = map[string]string{"Prefer": "respond-async"}
	opts.NoResponse = true

	id, _, _ := parseDirID(directoryID)

	replacedLeaf := replaceReservedChars(leaf)
	copyReq := api.CopyItemRequest{
		Name: &replacedLeaf,
		ParentReference: api.ItemReference{
			DriveID: f.driveID,
			ID:      id,
		},
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, &copyReq, nil)
		return shouldRetry(resp, err)
	})
	if err != nil {
		return nil, err
	}

	// read location header
	location := resp.Header.Get("Location")
	if location == "" {
		return nil, errors.New("didn't receive location header in copy response")
	}

	// Wait for job to finish
	err = f.waitForJob(location, dstObj)
	if err != nil {
		return nil, err
	}

	// Copy does NOT copy the modTime from the source and there seems to
	// be no way to set date before
	// This will create TWO versions on OneDrive
	err = dstObj.SetModTime(srcObj.ModTime())
	if err != nil {
		return nil, err
	}

	return dstObj, nil
}

// Purge deletes all the files and the container
//
// Optional interface: Only implement this if you have a way of
// deleting all the files quicker than just running Remove() on the
// result of List()
func (f *Fs) Purge() error {
	return f.purgeCheck("", false)
}

// Move src to this remote using server side move operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove
func (f *Fs) Move(src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	// Create temporary object
	dstObj, leaf, directoryID, err := f.createObject(remote, srcObj.modTime, srcObj.size)
	if err != nil {
		return nil, err
	}

	// Move the object
	opts := newOptsCall(srcObj.id, "PATCH", "")

	id, _, _ := parseDirID(directoryID)

	move := api.MoveItemRequest{
		Name: replaceReservedChars(leaf),
		ParentReference: &api.ItemReference{
			ID: id,
		},
		// We set the mod time too as it gets reset otherwise
		FileSystemInfo: &api.FileSystemInfoFacet{
			CreatedDateTime:      api.Timestamp(srcObj.modTime),
			LastModifiedDateTime: api.Timestamp(srcObj.modTime),
		},
	}
	var resp *http.Response
	var info api.Item
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, &move, &info)
		return shouldRetry(resp, err)
	})
	if err != nil {
		return nil, err
	}

	err = dstObj.setMetaData(&info)
	if err != nil {
		return nil, err
	}
	return dstObj, nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server side move operations.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantDirMove
//
// If destination exists then return fs.ErrorDirExists
func (f *Fs) DirMove(src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}
	srcPath := path.Join(srcFs.root, srcRemote)
	dstPath := path.Join(f.root, dstRemote)

	// Refuse to move to or from the root
	if srcPath == "" || dstPath == "" {
		fs.Debugf(src, "DirMove error: Can't move root")
		return errors.New("can't move root directory")
	}

	// find the root src directory
	err := srcFs.dirCache.FindRoot(false)
	if err != nil {
		return err
	}

	// find the root dst directory
	if dstRemote != "" {
		err = f.dirCache.FindRoot(true)
		if err != nil {
			return err
		}
	} else {
		if f.dirCache.FoundRoot() {
			return fs.ErrorDirExists
		}
	}

	// Find ID of dst parent, creating subdirs if necessary
	var leaf, dstDirectoryID string
	findPath := dstRemote
	if dstRemote == "" {
		findPath = f.root
	}
	leaf, dstDirectoryID, err = f.dirCache.FindPath(findPath, true)
	if err != nil {
		return err
	}
	parsedDstDirID, _, _ := parseDirID(dstDirectoryID)

	// Check destination does not exist
	if dstRemote != "" {
		_, err = f.dirCache.FindDir(dstRemote, false)
		if err == fs.ErrorDirNotFound {
			// OK
		} else if err != nil {
			return err
		} else {
			return fs.ErrorDirExists
		}
	}

	// Find ID of src
	srcID, err := srcFs.dirCache.FindDir(srcRemote, false)
	if err != nil {
		return err
	}

	// Get timestamps of src so they can be preserved
	srcInfo, _, err := srcFs.readMetaDataForPath(srcPath)
	if err != nil {
		return err
	}

	// Do the move
	opts := newOptsCall(srcID, "PATCH", "")
	move := api.MoveItemRequest{
		Name: replaceReservedChars(leaf),
		ParentReference: &api.ItemReference{
			ID: parsedDstDirID,
		},
		// We set the mod time too as it gets reset otherwise
		FileSystemInfo: &api.FileSystemInfoFacet{
			CreatedDateTime:      srcInfo.CreatedDateTime,
			LastModifiedDateTime: srcInfo.LastModifiedDateTime,
		},
	}
	var resp *http.Response
	var info api.Item
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, &move, &info)
		return shouldRetry(resp, err)
	})
	if err != nil {
		return err
	}

	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// DirCacheFlush resets the directory cache - used in testing as an
// optional interface
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// About gets quota information
func (f *Fs) About() (usage *fs.Usage, err error) {
	var drive api.Drive
	opts := rest.Opts{
		Method: "GET",
		Path:   "",
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, nil, &drive)
		return shouldRetry(resp, err)
	})
	if err != nil {
		return nil, errors.Wrap(err, "about failed")
	}
	q := drive.Quota
	usage = &fs.Usage{
		Total:   fs.NewUsageValue(q.Total),     // quota of bytes that can be used
		Used:    fs.NewUsageValue(q.Used),      // bytes in use
		Trashed: fs.NewUsageValue(q.Deleted),   // bytes in trash
		Free:    fs.NewUsageValue(q.Remaining), // bytes which can be uploaded before reaching the quota
	}
	return usage, nil
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	if f.driveType == driveTypePersonal {
		return hash.Set(hash.SHA1)
	}
	return hash.Set(hash.QuickXorHash)
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// srvPath returns a path for use in server
func (o *Object) srvPath() string {
	return replaceReservedChars(o.fs.rootSlash() + o.remote)
}

// Hash returns the SHA-1 of an object returning a lowercase hex string
func (o *Object) Hash(t hash.Type) (string, error) {
	if o.fs.driveType == driveTypePersonal {
		if t == hash.SHA1 {
			return o.sha1, nil
		}
	} else {
		if t == hash.QuickXorHash {
			return o.quickxorhash, nil
		}
	}
	return "", hash.ErrUnsupported
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	err := o.readMetaData()
	if err != nil {
		fs.Logf(o, "Failed to read metadata: %v", err)
		return 0
	}
	return o.size
}

// setMetaData sets the metadata from info
func (o *Object) setMetaData(info *api.Item) (err error) {
	if info.GetFolder() != nil {
		return errors.Wrapf(fs.ErrorNotAFile, "%q", o.remote)
	}
	o.hasMetaData = true
	o.size = info.GetSize()

	// Docs: https://docs.microsoft.com/en-us/onedrive/developer/rest-api/resources/hashes
	//
	// We use SHA1 for onedrive personal and QuickXorHash for onedrive for business
	file := info.GetFile()
	if file != nil {
		o.mimeType = file.MimeType
		if file.Hashes.Sha1Hash != "" {
			o.sha1 = strings.ToLower(file.Hashes.Sha1Hash)
		}
		if file.Hashes.QuickXorHash != "" {
			h, err := base64.StdEncoding.DecodeString(file.Hashes.QuickXorHash)
			if err != nil {
				fs.Errorf(o, "Failed to decode QuickXorHash %q: %v", file.Hashes.QuickXorHash, err)
			} else {
				o.quickxorhash = hex.EncodeToString(h)
			}
		}
	}
	fileSystemInfo := info.GetFileSystemInfo()
	if fileSystemInfo != nil {
		o.modTime = time.Time(fileSystemInfo.LastModifiedDateTime)
	} else {
		o.modTime = time.Time(info.GetLastModifiedDateTime())
	}
	o.id = info.GetID()
	return nil
}

// readMetaData gets the metadata if it hasn't already been fetched
//
// it also sets the info
func (o *Object) readMetaData() (err error) {
	if o.hasMetaData {
		return nil
	}
	info, _, err := o.fs.readMetaDataForPath(o.srvPath())
	if err != nil {
		if apiErr, ok := err.(*api.Error); ok {
			if apiErr.ErrorInfo.Code == "itemNotFound" {
				return fs.ErrorObjectNotFound
			}
		}
		return err
	}
	return o.setMetaData(info)
}

// ModTime returns the modification time of the object
//
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime() time.Time {
	err := o.readMetaData()
	if err != nil {
		fs.Logf(o, "Failed to read metadata: %v", err)
		return time.Now()
	}
	return o.modTime
}

// setModTime sets the modification time of the local fs object
func (o *Object) setModTime(modTime time.Time) (*api.Item, error) {
	var opts rest.Opts
	_, directoryID, _ := o.fs.dirCache.FindPath(o.remote, false)
	_, drive, rootURL := parseDirID(directoryID)
	if drive != "" {
		opts = rest.Opts{
			Method:  "PATCH",
			RootURL: rootURL,
			Path:    "/" + drive + "/root:/" + rest.URLPathEscape(o.srvPath()),
		}
	} else {
		opts = rest.Opts{
			Method: "PATCH",
			Path:   "/root:/" + rest.URLPathEscape(o.srvPath()),
		}
	}
	update := api.SetFileSystemInfo{
		FileSystemInfo: api.FileSystemInfoFacet{
			CreatedDateTime:      api.Timestamp(modTime),
			LastModifiedDateTime: api.Timestamp(modTime),
		},
	}
	var info *api.Item
	err := o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(&opts, &update, &info)
		return shouldRetry(resp, err)
	})
	return info, err
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(modTime time.Time) error {
	info, err := o.setModTime(modTime)
	if err != nil {
		return err
	}
	return o.setMetaData(info)
}

// Storable returns a boolean showing whether this object storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open(options ...fs.OpenOption) (in io.ReadCloser, err error) {
	if o.id == "" {
		return nil, errors.New("can't download - no id")
	}
	fs.FixRangeOption(options, o.size)
	var resp *http.Response
	opts := newOptsCall(o.id, "GET", "/content")
	opts.Options = options

	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.Call(&opts)
		return shouldRetry(resp, err)
	})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 && resp.Header.Get("Content-Range") == "" {
		//Overwrite size with actual size since size readings from Onedrive is unreliable.
		o.size = resp.ContentLength
	}
	return resp.Body, err
}

// createUploadSession creates an upload session for the object
func (o *Object) createUploadSession(modTime time.Time) (response *api.CreateUploadResponse, err error) {
	leaf, directoryID, _ := o.fs.dirCache.FindPath(o.remote, false)
	id, drive, rootURL := parseDirID(directoryID)
	var opts rest.Opts
	if drive != "" {
		opts = rest.Opts{
			Method:  "POST",
			RootURL: rootURL,
			Path:    "/" + drive + "/items/" + id + ":/" + rest.URLPathEscape(leaf) + ":/createUploadSession",
		}
	} else {
		opts = rest.Opts{
			Method: "POST",
			Path:   "/root:/" + rest.URLPathEscape(o.srvPath()) + ":/createUploadSession",
		}
	}
	createRequest := api.CreateUploadRequest{}
	createRequest.Item.FileSystemInfo.CreatedDateTime = api.Timestamp(modTime)
	createRequest.Item.FileSystemInfo.LastModifiedDateTime = api.Timestamp(modTime)
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.CallJSON(&opts, &createRequest, &response)
		return shouldRetry(resp, err)
	})
	return response, err
}

// uploadFragment uploads a part
func (o *Object) uploadFragment(url string, start int64, totalSize int64, chunk io.ReadSeeker, chunkSize int64) (info *api.Item, err error) {
	opts := rest.Opts{
		Method:        "PUT",
		RootURL:       url,
		ContentLength: &chunkSize,
		ContentRange:  fmt.Sprintf("bytes %d-%d/%d", start, start+chunkSize-1, totalSize),
		Body:          chunk,
	}
	//	var response api.UploadFragmentResponse
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		_, _ = chunk.Seek(0, io.SeekStart)
		resp, err = o.fs.srv.Call(&opts)
		if resp != nil {
			defer fs.CheckClose(resp.Body, &err)
		}
		retry, err := shouldRetry(resp, err)
		if !retry && resp != nil {
			if resp.StatusCode == 200 || resp.StatusCode == 201 {
				// we are done :)
				// read the item
				info = &api.Item{}
				return false, json.NewDecoder(resp.Body).Decode(info)
			}
		}
		return retry, err
	})
	return info, err
}

// cancelUploadSession cancels an upload session
func (o *Object) cancelUploadSession(url string) (err error) {
	opts := rest.Opts{
		Method:     "DELETE",
		RootURL:    url,
		NoResponse: true,
	}
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.Call(&opts)
		return shouldRetry(resp, err)
	})
	return
}

// uploadMultipart uploads a file using multipart upload
func (o *Object) uploadMultipart(in io.Reader, size int64, modTime time.Time) (info *api.Item, err error) {
	// Create upload session
	fs.Debugf(o, "Starting multipart upload")
	session, err := o.createUploadSession(modTime)
	if err != nil {
		return nil, err
	}
	uploadURL := session.UploadURL

	// Cancel the session if something went wrong
	defer func() {
		if err != nil {
			fs.Debugf(o, "Cancelling multipart upload: %v", err)
			cancelErr := o.cancelUploadSession(uploadURL)
			if cancelErr != nil {
				fs.Logf(o, "Failed to cancel multipart upload: %v", err)
			}
		}
	}()

	// Upload the chunks
	remaining := size
	position := int64(0)
	for remaining > 0 {
		n := int64(o.fs.opt.ChunkSize)
		if remaining < n {
			n = remaining
		}
		seg := readers.NewRepeatableReader(io.LimitReader(in, n))
		fs.Debugf(o, "Uploading segment %d/%d size %d", position, size, n)
		info, err = o.uploadFragment(uploadURL, position, size, seg, n)
		if err != nil {
			return nil, err
		}
		remaining -= n
		position += n
	}

	return info, nil
}

// Update the object with the contents of the io.Reader, modTime and size
//
// The new object may have been created if an error is returned
func (o *Object) Update(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	o.fs.tokenRenewer.Start()
	defer o.fs.tokenRenewer.Stop()

	size := src.Size()
	modTime := src.ModTime()

	info, err := o.uploadMultipart(in, size, modTime)
	if err != nil {
		return err
	}
	return o.setMetaData(info)
}

// Remove an object
func (o *Object) Remove() error {
	return o.fs.deleteObject(o.id)
}

// MimeType of an Object if known, "" otherwise
func (o *Object) MimeType() string {
	return o.mimeType
}

// ID returns the ID of the Object if known, or "" if not
func (o *Object) ID() string {
	return o.id
}

func newOptsCall(id string, method string, route string) (opts rest.Opts) {
	id, drive, rootURL := parseDirID(id)

	if drive != "" {
		return rest.Opts{
			Method:  method,
			RootURL: rootURL,
			Path:    "/" + drive + "/items/" + id + route,
		}
	}
	return rest.Opts{
		Method: method,
		Path:   "/items/" + id + route,
	}
}

func parseDirID(ID string) (string, string, string) {
	if strings.Index(ID, "#") >= 0 {
		s := strings.Split(ID, "#")
		return s[1], s[0], graphURL + "/drives"
	}
	return ID, "", ""
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.MimeTyper       = &Object{}
	_ fs.IDer            = &Object{}
)
