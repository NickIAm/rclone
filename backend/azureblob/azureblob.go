//go:build !plan9 && !solaris && !js
// +build !plan9,!solaris,!js

// Package azureblob provides an interface to the Microsoft Azure blob object storage system
package azureblob

/* FIXME

Note these Azure SDK bugs which are affecting the backend

azblob UploadStream produces panic: send on closed channel if input stream has error #19612
https://github.com/Azure/azure-sdk-for-go/issues/19612
   - FIXED by re-implementing UploadStream

azblob: when using SharedKey credentials, can't reference some blob names with ? in #19613
https://github.com/Azure/azure-sdk-for-go/issues/19613
   - FIXED by url encoding getBlobSVC and getBlockBlobSVC

Azure Blob Storage paths are not URL-escaped #19475
https://github.com/Azure/azure-sdk-for-go/issues/19475
   - FIXED by url encoding getBlobSVC and getBlockBlobSVC

Controlling TransferManager #19579
https://github.com/Azure/azure-sdk-for-go/issues/19579
   - FIXED by re-implementing UploadStream

azblob: blob.StartCopyFromURL doesn't work with UTF-8 characters in the source blob #19614
https://github.com/Azure/azure-sdk-for-go/issues/19614
   - FIXED by url encoding getBlobSVC and getBlockBlobSVC

*/

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/chunksize"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/walk"
	"github.com/rclone/rclone/lib/bucket"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/pool"
	"github.com/rclone/rclone/lib/readers"
	"golang.org/x/sync/errgroup"
)

const (
	minSleep              = 10 * time.Millisecond
	maxSleep              = 10 * time.Second
	decayConstant         = 1    // bigger for slower decay, exponential
	maxListChunkSize      = 5000 // number of items to read at once
	modTimeKey            = "mtime"
	timeFormatIn          = time.RFC3339
	timeFormatOut         = "2006-01-02T15:04:05.000000000Z07:00"
	storageDefaultBaseURL = "blob.core.windows.net"
	defaultChunkSize      = 4 * fs.Mebi
	defaultAccessTier     = blob.AccessTier("") // FIXME AccessTierNone
	// Default storage account, key and blob endpoint for emulator support,
	// though it is a base64 key checked in here, it is publicly available secret.
	emulatorAccount      = "devstoreaccount1"
	emulatorAccountKey   = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	emulatorBlobEndpoint = "http://127.0.0.1:10000/devstoreaccount1"
	memoryPoolFlushTime  = fs.Duration(time.Minute) // flush the cached buffers after this long
	memoryPoolUseMmap    = false
)

var (
	errCantUpdateArchiveTierBlobs = fserrors.NoRetryError(errors.New("can't update archive tier blob without --azureblob-archive-tier-delete"))
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "azureblob",
		Description: "Microsoft Azure Blob Storage",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name: "account",
			Help: "Storage Account Name.\n\nLeave blank to use SAS URL or Emulator.",
		}, {
			Name: "service_principal_file",
			Help: `Path to file containing credentials for use with a service principal.

Leave blank normally. Needed only if you want to use a service principal instead of interactive login.

    $ az ad sp create-for-rbac --name "<name>" \
      --role "Storage Blob Data Owner" \
      --scopes "/subscriptions/<subscription>/resourceGroups/<resource-group>/providers/Microsoft.Storage/storageAccounts/<storage-account>/blobServices/default/containers/<container>" \
      > azure-principal.json

See ["Create an Azure service principal"](https://docs.microsoft.com/en-us/cli/azure/create-an-azure-service-principal-azure-cli) and ["Assign an Azure role for access to blob data"](https://docs.microsoft.com/en-us/azure/storage/common/storage-auth-aad-rbac-cli) pages for more details.
`,
		}, {
			Name: "env_auth",
			Help: `Read credentials from runtime (environment variables).

Pull credentials from AZURE_TENANT_ID and AZURE_CLIENT_{ID,SECRET} environment vars.
See EnvironmentCredential in the Azure docs for more info.

Other authentication methods will, if specified, override this flag.`,
			Default: false,
		}, {
			Name: "key",
			Help: "Storage Account Key.\n\nLeave blank to use SAS URL or Emulator.",
		}, {
			Name: "sas_url",
			Help: "SAS URL for container level access only.\n\nLeave blank if using account/key or Emulator.",
		}, {
			Name: "use_msi",
			Help: `Use a managed service identity to authenticate (only works in Azure).

When true, use a [managed service identity](https://docs.microsoft.com/en-us/azure/active-directory/managed-identities-azure-resources/)
to authenticate to Azure Storage instead of a SAS token or account key.

If the VM(SS) on which this program is running has a system-assigned identity, it will
be used by default. If the resource has no system-assigned but exactly one user-assigned identity,
the user-assigned identity will be used by default. If the resource has multiple user-assigned
identities, the identity to use must be explicitly specified using exactly one of the msi_object_id,
msi_client_id, or msi_mi_res_id parameters.`,
			Default: false,
		}, {
			Name:     "msi_object_id",
			Help:     "Object ID of the user-assigned MSI to use, if any.\n\nLeave blank if msi_client_id or msi_mi_res_id specified.",
			Advanced: true,
		}, {
			Name:     "msi_client_id",
			Help:     "Object ID of the user-assigned MSI to use, if any.\n\nLeave blank if msi_object_id or msi_mi_res_id specified.",
			Advanced: true,
		}, {
			Name:     "msi_mi_res_id",
			Help:     "Azure resource ID of the user-assigned MSI to use, if any.\n\nLeave blank if msi_client_id or msi_object_id specified.",
			Advanced: true,
		}, {
			Name:    "use_emulator",
			Help:    "Uses local storage emulator if provided as 'true'.\n\nLeave blank if using real azure storage endpoint.",
			Default: false,
		}, {
			Name:     "endpoint",
			Help:     "Endpoint for the service.\n\nLeave blank normally.",
			Advanced: true,
		}, {
			Name:     "upload_cutoff",
			Help:     "Cutoff for switching to chunked upload (<= 256 MiB) (deprecated).",
			Advanced: true,
		}, {
			Name: "chunk_size",
			Help: `Upload chunk size.

Note that this is stored in memory and there may be up to
"--transfers" * "--azureblob-upload-concurrency" chunks stored at once
in memory.`,
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name: "upload_concurrency",
			Help: `Concurrency for multipart uploads.

This is the number of chunks of the same file that are uploaded
concurrently.

If you are uploading small numbers of large files over high-speed
links and these uploads do not fully utilize your bandwidth, then
increasing this may help to speed up the transfers.

In tests, upload speed increases almost linearly with upload
concurrency. For example to fill a gigabit pipe it may be necessary to
raise this to 64. Note that this will use more memory.

Note that chunks are stored in memory and there may be up to
"--transfers" * "--azureblob-upload-concurrency" chunks stored at once
in memory.`,
			Default:  16,
			Advanced: true,
		}, {
			Name: "list_chunk",
			Help: `Size of blob list.

This sets the number of blobs requested in each listing chunk. Default
is the maximum, 5000. "List blobs" requests are permitted 2 minutes
per megabyte to complete. If an operation is taking longer than 2
minutes per megabyte on average, it will time out (
[source](https://docs.microsoft.com/en-us/rest/api/storageservices/setting-timeouts-for-blob-service-operations#exceptions-to-default-timeout-interval)
). This can be used to limit the number of blobs items to return, to
avoid the time out.`,
			Default:  maxListChunkSize,
			Advanced: true,
		}, {
			Name: "access_tier",
			Help: `Access tier of blob: hot, cool or archive.

Archived blobs can be restored by setting access tier to hot or
cool. Leave blank if you intend to use default access tier, which is
set at account level

If there is no "access tier" specified, rclone doesn't apply any tier.
rclone performs "Set Tier" operation on blobs while uploading, if objects
are not modified, specifying "access tier" to new one will have no effect.
If blobs are in "archive tier" at remote, trying to perform data transfer
operations from remote will not be allowed. User should first restore by
tiering blob to "Hot" or "Cool".`,
			Advanced: true,
		}, {
			Name:    "archive_tier_delete",
			Default: false,
			Help: fmt.Sprintf(`Delete archive tier blobs before overwriting.

Archive tier blobs cannot be updated. So without this flag, if you
attempt to update an archive tier blob, then rclone will produce the
error:

    %v

With this flag set then before rclone attempts to overwrite an archive
tier blob, it will delete the existing blob before uploading its
replacement.  This has the potential for data loss if the upload fails
(unlike updating a normal blob) and also may cost more since deleting
archive tier blobs early may be chargable.
`, errCantUpdateArchiveTierBlobs),
			Advanced: true,
		}, {
			Name: "disable_checksum",
			Help: `Don't store MD5 checksum with object metadata.

Normally rclone will calculate the MD5 checksum of the input before
uploading it so it can add it to metadata on the object. This is great
for data integrity checking but can cause long delays for large files
to start uploading.`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "memory_pool_flush_time",
			Default:  memoryPoolFlushTime,
			Advanced: true,
			Help: `How often internal memory buffer pools will be flushed.

Uploads which requires additional buffers (f.e multipart) will use memory pool for allocations.
This option controls how often unused buffers will be removed from the pool.`,
		}, {
			Name:     "memory_pool_use_mmap",
			Default:  memoryPoolUseMmap,
			Advanced: true,
			Help:     `Whether to use mmap buffers in internal memory pool.`,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default: (encoder.EncodeInvalidUtf8 |
				encoder.EncodeSlash |
				encoder.EncodeCtl |
				encoder.EncodeDel |
				encoder.EncodeBackSlash |
				encoder.EncodeRightPeriod),
		}, {
			Name:    "public_access",
			Help:    "Public access level of a container: blob or container.",
			Default: "",
			Examples: []fs.OptionExample{
				{
					Value: "",
					Help:  "The container and its blobs can be accessed only with an authorized request.\nIt's a default value.",
				}, {
					Value: string(container.PublicAccessTypeBlob),
					Help:  "Blob data within this container can be read via anonymous request.",
				}, {
					Value: string(container.PublicAccessTypeContainer),
					Help:  "Allow full public read access for container and blob data.",
				},
			},
			Advanced: true,
		}, {
			Name:     "no_head_object",
			Help:     `If set, do not do HEAD before GET when getting objects.`,
			Default:  false,
			Advanced: true,
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	Account              string               `config:"account"`
	ServicePrincipalFile string               `config:"service_principal_file"`
	EnvAuth              bool                 `config:"env_auth"`
	Key                  string               `config:"key"`
	UseMSI               bool                 `config:"use_msi"`
	MSIObjectID          string               `config:"msi_object_id"`
	MSIClientID          string               `config:"msi_client_id"`
	MSIResourceID        string               `config:"msi_mi_res_id"`
	Endpoint             string               `config:"endpoint"`
	SASURL               string               `config:"sas_url"`
	ChunkSize            fs.SizeSuffix        `config:"chunk_size"`
	UploadConcurrency    int                  `config:"upload_concurrency"`
	ListChunkSize        uint                 `config:"list_chunk"`
	AccessTier           string               `config:"access_tier"`
	ArchiveTierDelete    bool                 `config:"archive_tier_delete"`
	UseEmulator          bool                 `config:"use_emulator"`
	DisableCheckSum      bool                 `config:"disable_checksum"`
	MemoryPoolFlushTime  fs.Duration          `config:"memory_pool_flush_time"`
	MemoryPoolUseMmap    bool                 `config:"memory_pool_use_mmap"`
	Enc                  encoder.MultiEncoder `config:"encoding"`
	PublicAccess         string               `config:"public_access"`
	NoHeadObject         bool                 `config:"no_head_object"`
}

// Fs represents a remote azure server
type Fs struct {
	name          string                       // name of this remote
	root          string                       // the path we are working on if any
	opt           Options                      // parsed config options
	ci            *fs.ConfigInfo               // global config
	features      *fs.Features                 // optional features
	cntSVCcacheMu sync.Mutex                   // mutex to protect cntSVCcache
	cntSVCcache   map[string]*container.Client // reference to containerClient per container
	svc           *service.Client              // client to access azblob
	rootContainer string                       // container part of root (if any)
	rootDirectory string                       // directory part of root (if any)
	isLimited     bool                         // if limited to one container
	cache         *bucket.Cache                // cache for container creation status
	pacer         *fs.Pacer                    // To pace and retry the API calls
	uploadToken   *pacer.TokenDispenser        // control concurrency
	pool          *pool.Pool                   // memory pool
	poolSize      int64                        // size of pages in memory pool
	publicAccess  container.PublicAccessType   // Container Public Access Level
}

// Object describes an azure object
type Object struct {
	fs         *Fs               // what this object is part of
	remote     string            // The remote path
	modTime    time.Time         // The modified time of the object if known
	md5        string            // MD5 hash if known
	size       int64             // Size of the object
	mimeType   string            // Content-Type of the object
	accessTier blob.AccessTier   // Blob Access Tier
	meta       map[string]string // blob metadata
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
	if f.rootContainer == "" {
		return "Azure root"
	}
	if f.rootDirectory == "" {
		return fmt.Sprintf("Azure container %s", f.rootContainer)
	}
	return fmt.Sprintf("Azure container %s path %s", f.rootContainer, f.rootDirectory)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// parsePath parses a remote 'url'
func parsePath(path string) (root string) {
	root = strings.Trim(path, "/")
	return
}

// split returns container and containerPath from the rootRelativePath
// relative to f.root
func (f *Fs) split(rootRelativePath string) (containerName, containerPath string) {
	containerName, containerPath = bucket.Split(path.Join(f.root, rootRelativePath))
	return f.opt.Enc.FromStandardName(containerName), f.opt.Enc.FromStandardPath(containerPath)
}

// split returns container and containerPath from the object
func (o *Object) split() (container, containerPath string) {
	return o.fs.split(o.remote)
}

// validateAccessTier checks if azureblob supports user supplied tier
func validateAccessTier(tier string) bool {
	return strings.EqualFold(tier, string(blob.AccessTierHot)) ||
		strings.EqualFold(tier, string(blob.AccessTierCool)) ||
		strings.EqualFold(tier, string(blob.AccessTierArchive))
}

// validatePublicAccess checks if azureblob supports use supplied public access level
func validatePublicAccess(publicAccess string) bool {
	switch publicAccess {
	case "",
		string(container.PublicAccessTypeBlob),
		string(container.PublicAccessTypeContainer):
		// valid cases
		return true
	default:
		return false
	}
}

// retryErrorCodes is a slice of error codes that we will retry
var retryErrorCodes = []int{
	401, // Unauthorized (e.g. "Token has expired")
	408, // Request Timeout
	429, // Rate exceeded.
	500, // Get occasional 500 Internal Server Error
	503, // Service Unavailable
	504, // Gateway Time-out
}

// shouldRetry returns a boolean as to whether this resp and err
// deserve to be retried.  It returns the err as a convenience
func (f *Fs) shouldRetry(ctx context.Context, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	// FIXME interpret special errors - more to do here
	if storageErr, ok := err.(*azcore.ResponseError); ok {
		switch storageErr.ErrorCode {
		case "InvalidBlobOrBlock":
			// These errors happen sometimes in multipart uploads
			// because of block concurrency issues
			return true, err
		}
		statusCode := storageErr.StatusCode
		for _, e := range retryErrorCodes {
			if statusCode == e {
				return true, err
			}
		}
	}
	return fserrors.ShouldRetry(err), err
}

func checkUploadChunkSize(cs fs.SizeSuffix) error {
	const minChunkSize = fs.SizeSuffixBase
	if cs < minChunkSize {
		return fmt.Errorf("%s is less than %s", cs, minChunkSize)
	}
	return nil
}

func (f *Fs) setUploadChunkSize(cs fs.SizeSuffix) (old fs.SizeSuffix, err error) {
	err = checkUploadChunkSize(cs)
	if err == nil {
		old, f.opt.ChunkSize = f.opt.ChunkSize, cs
	}
	return
}

type servicePrincipalCredentials struct {
	AppID    string `json:"appId"`
	Password string `json:"password"`
	Tenant   string `json:"tenant"`
}

// setRoot changes the root of the Fs
func (f *Fs) setRoot(root string) {
	f.root = parsePath(root)
	f.rootContainer, f.rootDirectory = bucket.Split(f.root)
}

// Wrap the http.Transport to satisfy the Transporter interface
type transporter struct {
	http.RoundTripper
}

// Make a new transporter
func newTransporter(ctx context.Context) transporter {
	return transporter{
		RoundTripper: fshttp.NewTransport(ctx),
	}
}

// Do sends the HTTP request and returns the HTTP response or error.
func (tr transporter) Do(req *http.Request) (*http.Response, error) {
	return tr.RoundTripper.RoundTrip(req)
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	err = checkUploadChunkSize(opt.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("chunk size: %w", err)
	}
	if opt.ListChunkSize > maxListChunkSize {
		return nil, fmt.Errorf("blob list size can't be greater than %v - was %v", maxListChunkSize, opt.ListChunkSize)
	}
	if opt.Endpoint == "" {
		opt.Endpoint = storageDefaultBaseURL
	}

	if opt.AccessTier == "" {
		opt.AccessTier = string(defaultAccessTier)
	} else if !validateAccessTier(opt.AccessTier) {
		return nil, fmt.Errorf("supported access tiers are %s, %s and %s",
			string(blob.AccessTierHot), string(blob.AccessTierCool), string(blob.AccessTierArchive))
	}

	if !validatePublicAccess((opt.PublicAccess)) {
		return nil, fmt.Errorf("supported public access level are %s and %s",
			string(container.PublicAccessTypeBlob), string(container.PublicAccessTypeContainer))
	}

	ci := fs.GetConfig(ctx)
	f := &Fs{
		name:        name,
		opt:         *opt,
		ci:          ci,
		pacer:       fs.NewPacer(ctx, pacer.NewS3(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decayConstant))),
		uploadToken: pacer.NewTokenDispenser(ci.Transfers),
		cache:       bucket.NewCache(),
		cntSVCcache: make(map[string]*container.Client, 1),
		pool: pool.New(
			time.Duration(opt.MemoryPoolFlushTime),
			int(opt.ChunkSize),
			ci.Transfers,
			opt.MemoryPoolUseMmap,
		),
		poolSize: int64(opt.ChunkSize),
	}
	f.publicAccess = container.PublicAccessType(opt.PublicAccess)
	f.setRoot(root)
	f.features = (&fs.Features{
		ReadMimeType:      true,
		WriteMimeType:     true,
		BucketBased:       true,
		BucketBasedRootOK: true,
		SetTier:           true,
		GetTier:           true,
	}).Fill(ctx, f)

	// var (
	// 	u          *url.URL
	// 	serviceURL azblob.ServiceURL
	// )
	// switch {
	// case opt.UseEmulator:
	// 	var actualEmulatorAccount = emulatorAccount
	// 	if opt.Account != "" {
	// 		actualEmulatorAccount = opt.Account
	// 	}
	// 	var actualEmulatorKey = emulatorAccountKey
	// 	if opt.Key != "" {
	// 		actualEmulatorKey = opt.Key
	// 	}
	// 	credential, err := azblob.NewSharedKeyCredential(actualEmulatorAccount, actualEmulatorKey)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	// 	}
	// 	var actualEmulatorEndpoint = emulatorBlobEndpoint
	// 	if opt.Endpoint != "" {
	// 		actualEmulatorEndpoint = opt.Endpoint
	// 	}
	// 	u, err = url.Parse(actualEmulatorEndpoint)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to make azure storage url from account and endpoint: %w", err)
	// 	}
	// 	pipeline := f.newPipeline(credential, azblob.PipelineOptions{Retry: azblob.RetryOptions{TryTimeout: maxTryTimeout}})
	// 	serviceURL = azblob.NewServiceURL(*u, pipeline)
	// case opt.UseMSI:
	// 	var token adal.Token
	// 	var userMSI = &userMSI{}
	// 	if len(opt.MSIClientID) > 0 || len(opt.MSIObjectID) > 0 || len(opt.MSIResourceID) > 0 {
	// 		// Specifying a user-assigned identity. Exactly one of the above IDs must be specified.
	// 		// Validate and ensure exactly one is set. (To do: better validation.)
	// 		if len(opt.MSIClientID) > 0 {
	// 			if len(opt.MSIObjectID) > 0 || len(opt.MSIResourceID) > 0 {
	// 				return nil, errors.New("more than one user-assigned identity ID is set")
	// 			}
	// 			userMSI.Type = msiClientID
	// 			userMSI.Value = opt.MSIClientID
	// 		}
	// 		if len(opt.MSIObjectID) > 0 {
	// 			if len(opt.MSIClientID) > 0 || len(opt.MSIResourceID) > 0 {
	// 				return nil, errors.New("more than one user-assigned identity ID is set")
	// 			}
	// 			userMSI.Type = msiObjectID
	// 			userMSI.Value = opt.MSIObjectID
	// 		}
	// 		if len(opt.MSIResourceID) > 0 {
	// 			if len(opt.MSIClientID) > 0 || len(opt.MSIObjectID) > 0 {
	// 				return nil, errors.New("more than one user-assigned identity ID is set")
	// 			}
	// 			userMSI.Type = msiResourceID
	// 			userMSI.Value = opt.MSIResourceID
	// 		}
	// 	} else {
	// 		userMSI = nil
	// 	}
	// 	err = f.imdsPacer.Call(func() (bool, error) {
	// 		// Retry as specified by the documentation:
	// 		// https://docs.microsoft.com/en-us/azure/active-directory/managed-identities-azure-resources/how-to-use-vm-token#retry-guidance
	// 		token, err = GetMSIToken(ctx, userMSI)
	// 		return f.shouldRetry(ctx, err)
	// 	})

	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to acquire MSI token: %w", err)
	// 	}

	// 	u, err = url.Parse(fmt.Sprintf("https://%s.%s", opt.Account, opt.Endpoint))
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to make azure storage url from account and endpoint: %w", err)
	// 	}
	// 	credential := azblob.NewTokenCredential(token.AccessToken, func(credential azblob.TokenCredential) time.Duration {
	// 		fs.Debugf(f, "Token refresher called.")
	// 		var refreshedToken adal.Token
	// 		err := f.imdsPacer.Call(func() (bool, error) {
	// 			refreshedToken, err = GetMSIToken(ctx, userMSI)
	// 			return f.shouldRetry(ctx, err)
	// 		})
	// 		if err != nil {
	// 			// Failed to refresh.
	// 			return 0
	// 		}
	// 		credential.SetToken(refreshedToken.AccessToken)
	// 		now := time.Now().UTC()
	// 		// Refresh one minute before expiry.
	// 		refreshAt := refreshedToken.Expires().UTC().Add(-1 * time.Minute)
	// 		fs.Debugf(f, "Acquired new token that expires at %v; refreshing in %d s", refreshedToken.Expires(),
	// 			int(refreshAt.Sub(now).Seconds()))
	// 		if now.After(refreshAt) {
	// 			// Acquired a causality violation.
	// 			return 0
	// 		}
	// 		return refreshAt.Sub(now)
	// 	})
	// 	pipeline := f.newPipeline(credential, azblob.PipelineOptions{Retry: azblob.RetryOptions{TryTimeout: maxTryTimeout}})
	// 	serviceURL = azblob.NewServiceURL(*u, pipeline)
	// case opt.Account != "" && opt.Key != "":
	// 	credential, err := azblob.NewSharedKeyCredential(opt.Account, opt.Key)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	// 	}

	// 	u, err = url.Parse(fmt.Sprintf("https://%s.%s", opt.Account, opt.Endpoint))
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to make azure storage url from account and endpoint: %w", err)
	// 	}
	// 	pipeline := f.newPipeline(credential, azblob.PipelineOptions{Retry: azblob.RetryOptions{TryTimeout: maxTryTimeout}})
	// 	serviceURL = azblob.NewServiceURL(*u, pipeline)
	// case opt.SASURL != "":
	// 	u, err = url.Parse(opt.SASURL)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to parse SAS URL: %w", err)
	// 	}
	// 	// use anonymous credentials in case of sas url
	// 	pipeline := f.newPipeline(azblob.NewAnonymousCredential(), azblob.PipelineOptions{Retry: azblob.RetryOptions{TryTimeout: maxTryTimeout}})
	// 	// Check if we have container level SAS or account level sas
	// 	parts := azblob.NewBlobURLParts(*u)
	// 	if parts.ContainerName != "" {
	// 		if f.rootContainer != "" && parts.ContainerName != f.rootContainer {
	// 			return nil, errors.New("container name in SAS URL and container provided in command do not match")
	// 		}
	// 		containerURL := azblob.NewContainerURL(*u, pipeline)
	// 		f.cntSVCcache[parts.ContainerName] = &containerURL
	// 		f.isLimited = true
	// 	} else {
	// 		serviceURL = azblob.NewServiceURL(*u, pipeline)
	// 	}
	// case opt.ServicePrincipalFile != "":
	// 	// Create a standard URL.
	// 	u, err = url.Parse(fmt.Sprintf("https://%s.%s", opt.Account, opt.Endpoint))
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to make azure storage url from account and endpoint: %w", err)
	// 	}
	// 	// Try loading service principal credentials from file.
	// 	loadedCreds, err := os.ReadFile(env.ShellExpand(opt.ServicePrincipalFile))
	// 	if err != nil {
	// 		return nil, fmt.Errorf("error opening service principal credentials file: %w", err)
	// 	}
	// 	// Create a token refresher from service principal credentials.
	// 	tokenRefresher, err := newServicePrincipalTokenRefresher(ctx, loadedCreds)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("failed to create a service principal token: %w", err)
	// 	}
	// 	options := azblob.PipelineOptions{Retry: azblob.RetryOptions{TryTimeout: maxTryTimeout}}
	// 	pipe := f.newPipeline(azblob.NewTokenCredential("", tokenRefresher), options)
	// 	serviceURL = azblob.NewServiceURL(*u, pipe)
	// default:
	// 	return nil, errors.New("no authentication method configured")
	// }
	//f.svcURL = &serviceURL

	u, err := url.Parse(fmt.Sprintf("https://%s.%s", opt.Account, opt.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to make azure storage url from account and endpoint: %w", err)
	}
	serviceURL := u.String()
	fs.Debugf(f, "Service URL = %q", serviceURL)

	// FIXME Very quick and dirty auth

	var (
		cred          azcore.TokenCredential
		sharedKeyCred *service.SharedKeyCredential
	)

	if opt.EnvAuth {
		// Read credentials from the environment
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("create default azure credential failed: %w", err)
		}
	} else {

		// Use the config file to configure
		sharedKeyCred, err = service.NewSharedKeyCredential(opt.Account, opt.Key)
		if err != nil {
			return nil, fmt.Errorf("create new shared key credential failed: %w", err)
		}
	}

	// Specify our own transport
	clientOpt := service.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: newTransporter(ctx),
		},
	}
	// azClientOpt := azblob.ClientOptions{
	// 	ClientOptions: azcore.ClientOptions{
	// 		Transport: clientOpt.ClientOptions.Transport,
	// 	},
	// }

	if sharedKeyCred != nil {
		// create a client for the specified storage account
		client, err := service.NewClientWithSharedKeyCredential(serviceURL, sharedKeyCred, &clientOpt)
		if err != nil {
			return nil, fmt.Errorf("create client with shared key failed: %w", err)
		}
		f.svc = client

		// create a client for the specified storage account
		//
		// Annoyingly this is the same type as f.svc just wrapped in a
		// struct, but there is no way to create one from the other.
		// azsvc, err := azblob.NewClientWithSharedKeyCredential(serviceURL, sharedKeyCred, &azClientOpt)
		// if err != nil {
		// 	return nil, fmt.Errorf("create client failed: %w", err)
		// }
		// f.azsvc = azsvc
	} else {
		// create a client for the specified storage account
		// azblob.ClientOptions{}
		client, err := service.NewClient(serviceURL, cred, &clientOpt)
		if err != nil {
			return nil, fmt.Errorf("create client failed: %w", err)
		}
		f.svc = client

		// create a client for the specified storage account
		// azblob.ClientOptions{}
		//
		// Annoyingly this is the same type as f.svc just wrapped in a
		// struct, but there is no way to create one from the other.
		// azsvc, err := azblob.NewClient(serviceURL, cred, &azClientOpt)
		// if err != nil {
		// 	return nil, fmt.Errorf("create client failed: %w", err)
		// }
		// f.azsvc = azsvc
	}

	if f.rootContainer != "" && f.rootDirectory != "" {
		// Check to see if the (container,directory) is actually an existing file
		oldRoot := f.root
		newRoot, leaf := path.Split(oldRoot)
		f.setRoot(newRoot)
		_, err := f.NewObject(ctx, leaf)
		if err != nil {
			if err == fs.ErrorObjectNotFound || err == fs.ErrorNotAFile {
				// File doesn't exist or is a directory so return old f
				f.setRoot(oldRoot)
				return f, nil
			}
			return nil, err
		}
		// return an error with an fs which points to the parent
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// return the container client for the container passed in
func (f *Fs) cntSVC(containerName string) (containerClient *container.Client) {
	f.cntSVCcacheMu.Lock()
	defer f.cntSVCcacheMu.Unlock()
	var ok bool
	if containerClient, ok = f.cntSVCcache[containerName]; !ok {
		containerClient = f.svc.NewContainerClient(containerName)
		f.cntSVCcache[containerName] = containerClient
	}
	return containerClient
}

// Return an Object from a path
//
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) newObjectWithInfo(remote string, info *container.BlobItem) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	if info != nil {
		err := o.decodeMetaDataFromBlob(info)
		if err != nil {
			return nil, err
		}
	} else if !o.fs.opt.NoHeadObject {
		err := o.readMetaData() // reads info and headers, returning an error
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return f.newObjectWithInfo(remote, nil)
}

// getBlobSVC creates a blob client
func (f *Fs) getBlobSVC(container, containerPath string) *blob.Client {
	// FIXME the urlEncode here is a workaround for
	// https://github.com/Azure/azure-sdk-for-go/issues/19613
	// https://github.com/Azure/azure-sdk-for-go/issues/19475
	return f.cntSVC(container).NewBlobClient(urlEncode(containerPath))
}

// getBlockBlobSVC creates a block blob client
func (f *Fs) getBlockBlobSVC(container, containerPath string) *blockblob.Client {
	// FIXME the urlEncode here is a workaround for
	// https://github.com/Azure/azure-sdk-for-go/issues/19613
	// https://github.com/Azure/azure-sdk-for-go/issues/19475
	return f.cntSVC(container).NewBlockBlobClient(urlEncode(containerPath))
}

// updateMetadataWithModTime adds the modTime passed in to o.meta.
func (o *Object) updateMetadataWithModTime(modTime time.Time) {
	// Make sure o.meta is not nil
	if o.meta == nil {
		o.meta = make(map[string]string, 1)
	}

	// Set modTimeKey in it
	o.meta[modTimeKey] = modTime.Format(timeFormatOut)
}

// Returns whether file is a directory marker or not
func isDirectoryMarker(size int64, metadata map[string]string, remote string) bool {
	// Directory markers are 0 length
	if size == 0 {
		// Note that metadata with hdi_isfolder = true seems to be a
		// defacto standard for marking blobs as directories.
		endsWithSlash := strings.HasSuffix(remote, "/")
		if endsWithSlash || remote == "" || metadata["hdi_isfolder"] == "true" {
			return true
		}

	}
	return false
}

// Returns whether file is a directory marker or not using metadata
// with pointers to strings as the SDK seems to use both forms rather
// annoyingly.
//
// NB This is a duplicate of isDirectoryMarker
func isDirectoryMarkerP(size int64, metadata map[string]*string, remote string) bool {
	// Directory markers are 0 length
	if size == 0 {
		// Note that metadata with hdi_isfolder = true seems to be a
		// defacto standard for marking blobs as directories.
		endsWithSlash := strings.HasSuffix(remote, "/")
		if endsWithSlash || remote == "" || (metadata["hdi_isfolder"] != nil && *metadata["hdi_isfolder"] == "true") {
			return true
		}

	}
	return false
}

// listFn is called from list to handle an object
type listFn func(remote string, object *container.BlobItem, isDirectory bool) error

// list lists the objects into the function supplied from
// the container and root supplied
//
// dir is the starting directory, "" for root
//
// The remote has prefix removed from it and if addContainer is set then
// it adds the container to the start.
func (f *Fs) list(ctx context.Context, containerName, directory, prefix string, addContainer bool, recurse bool, maxResults int32, fn listFn) error {
	if f.cache.IsDeleted(containerName) {
		return fs.ErrorDirNotFound
	}
	if prefix != "" {
		prefix += "/"
	}
	if directory != "" {
		directory += "/"
	}
	delimiter := ""
	if !recurse {
		delimiter = "/"
	}

	pager := f.cntSVC(containerName).NewListBlobsHierarchyPager(delimiter, &container.ListBlobsHierarchyOptions{
		// Copy, Metadata, Snapshots, UncommittedBlobs, Deleted, Tags, Versions, LegalHold, ImmutabilityPolicy, DeletedWithVersions bool
		Include: container.ListBlobsInclude{
			Copy:             false,
			Metadata:         true,
			Snapshots:        false,
			UncommittedBlobs: false,
			Deleted:          false,
		},
		Prefix:     &directory,
		MaxResults: &maxResults,
	})
	for pager.More() {
		var response container.ListBlobsHierarchyResponse
		err := f.pacer.Call(func() (bool, error) {
			var err error
			response, err = pager.NextPage(ctx)
			//response, err = f.srv.ListBlobsHierarchySegment(ctx, marker, delimiter, options)
			return f.shouldRetry(ctx, err)
		})

		if err != nil {
			// Check http error code along with service code, current SDK doesn't populate service code correctly sometimes
			if storageErr, ok := err.(*azcore.ResponseError); ok && (storageErr.ErrorCode == string(bloberror.ContainerNotFound) || storageErr.StatusCode == http.StatusNotFound) {
				return fs.ErrorDirNotFound
			}
			return err
		}
		// Advance marker to next
		// marker = response.NextMarker
		for i := range response.Segment.BlobItems {
			file := response.Segment.BlobItems[i]
			// Finish if file name no longer has prefix
			// if prefix != "" && !strings.HasPrefix(file.Name, prefix) {
			// 	return nil
			// }
			if file.Name == nil {
				fs.Debugf(f, "Nil name received")
				continue
			}
			remote := f.opt.Enc.ToStandardPath(*file.Name)
			if !strings.HasPrefix(remote, prefix) {
				fs.Debugf(f, "Odd name received %q", remote)
				continue
			}
			remote = remote[len(prefix):]
			if isDirectoryMarkerP(*file.Properties.ContentLength, file.Metadata, remote) {
				continue // skip directory marker
			}
			if addContainer {
				remote = path.Join(containerName, remote)
			}
			// Send object
			err = fn(remote, file, false)
			if err != nil {
				return err
			}
		}
		// Send the subdirectories
		for _, remote := range response.Segment.BlobPrefixes {
			if remote.Name == nil {
				fs.Debugf(f, "Nil prefix received")
				continue
			}
			remote := strings.TrimRight(*remote.Name, "/")
			remote = f.opt.Enc.ToStandardPath(remote)
			if !strings.HasPrefix(remote, prefix) {
				fs.Debugf(f, "Odd directory name received %q", remote)
				continue
			}
			remote = remote[len(prefix):]
			if addContainer {
				remote = path.Join(containerName, remote)
			}
			// Send object
			err = fn(remote, nil, true)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Convert a list item into a DirEntry
func (f *Fs) itemToDirEntry(remote string, object *container.BlobItem, isDirectory bool) (fs.DirEntry, error) {
	if isDirectory {
		d := fs.NewDir(remote, time.Time{})
		return d, nil
	}
	o, err := f.newObjectWithInfo(remote, object)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// Check to see if this is a limited container and the container is not found
func (f *Fs) containerOK(container string) bool {
	if !f.isLimited {
		return true
	}
	f.cntSVCcacheMu.Lock()
	defer f.cntSVCcacheMu.Unlock()
	for limitedContainer := range f.cntSVCcache {
		if container == limitedContainer {
			return true
		}
	}
	return false
}

// listDir lists a single directory
func (f *Fs) listDir(ctx context.Context, containerName, directory, prefix string, addContainer bool) (entries fs.DirEntries, err error) {
	if !f.containerOK(containerName) {
		return nil, fs.ErrorDirNotFound
	}
	err = f.list(ctx, containerName, directory, prefix, addContainer, false, int32(f.opt.ListChunkSize), func(remote string, object *container.BlobItem, isDirectory bool) error {
		entry, err := f.itemToDirEntry(remote, object, isDirectory)
		if err != nil {
			return err
		}
		if entry != nil {
			entries = append(entries, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// container must be present if listing succeeded
	f.cache.MarkOK(containerName)
	return entries, nil
}

// listContainers returns all the containers to out
func (f *Fs) listContainers(ctx context.Context) (entries fs.DirEntries, err error) {
	if f.isLimited {
		f.cntSVCcacheMu.Lock()
		for container := range f.cntSVCcache {
			d := fs.NewDir(container, time.Time{})
			entries = append(entries, d)
		}
		f.cntSVCcacheMu.Unlock()
		return entries, nil
	}
	err = f.listContainersToFn(func(Name string, LastModified time.Time) error {
		d := fs.NewDir(f.opt.Enc.ToStandardName(Name), LastModified)
		f.cache.MarkOK(Name)
		entries = append(entries, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
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
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	container, directory := f.split(dir)
	if container == "" {
		if directory != "" {
			return nil, fs.ErrorListBucketRequired
		}
		return f.listContainers(ctx)
	}
	return f.listDir(ctx, container, directory, f.rootDirectory, f.rootContainer == "")
}

// ListR lists the objects and directories of the Fs starting
// from dir recursively into out.
//
// dir should be "" to start from the root, and should not
// have trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
//
// It should call callback for each tranche of entries read.
// These need not be returned in any particular order.  If
// callback returns an error then the listing will stop
// immediately.
//
// Don't implement this unless you have a more efficient way
// of listing recursively that doing a directory traversal.
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) (err error) {
	containerName, directory := f.split(dir)
	list := walk.NewListRHelper(callback)
	listR := func(containerName, directory, prefix string, addContainer bool) error {
		return f.list(ctx, containerName, directory, prefix, addContainer, true, int32(f.opt.ListChunkSize), func(remote string, object *container.BlobItem, isDirectory bool) error {
			entry, err := f.itemToDirEntry(remote, object, isDirectory)
			if err != nil {
				return err
			}
			return list.Add(entry)
		})
	}
	if containerName == "" {
		entries, err := f.listContainers(ctx)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			err = list.Add(entry)
			if err != nil {
				return err
			}
			container := entry.Remote()
			err = listR(container, "", f.rootDirectory, true)
			if err != nil {
				return err
			}
			// container must be present if listing succeeded
			f.cache.MarkOK(container)
		}
	} else {
		if !f.containerOK(containerName) {
			return fs.ErrorDirNotFound
		}
		err = listR(containerName, directory, f.rootDirectory, f.rootContainer == "")
		if err != nil {
			return err
		}
		// container must be present if listing succeeded
		f.cache.MarkOK(containerName)
	}
	return list.Flush()
}

// listContainerFn is called from listContainersToFn to handle a container
type listContainerFn func(Name string, LastModified time.Time) error

// listContainersToFn lists the containers to the function supplied
func (f *Fs) listContainersToFn(fn listContainerFn) error {
	max := int32(f.opt.ListChunkSize)
	pager := f.svc.NewListContainersPager(&service.ListContainersOptions{
		Include:    service.ListContainersInclude{Metadata: true, Deleted: true},
		MaxResults: &max,
	})
	ctx := context.Background()
	for pager.More() {
		var response service.ListContainersResponse
		err := f.pacer.Call(func() (bool, error) {
			var err error
			response, err = pager.NextPage(ctx)
			return f.shouldRetry(ctx, err)
		})
		if err != nil {
			return err
		}

		for _, cnt := range response.ContainerItems {
			if cnt == nil || cnt.Name == nil || cnt.Properties == nil || cnt.Properties.LastModified == nil {
				fs.Debugf(f, "nil returned in container info")
			}
			err = fn(*cnt.Name, *cnt.Properties.LastModified)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Put the object into the container
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Temporary Object under construction
	fs := &Object{
		fs:     f,
		remote: src.Remote(),
	}
	return fs, fs.Update(ctx, in, src, options...)
}

// PutStream uploads to the remote path with the modTime given of indeterminate size
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	container, _ := f.split(dir)
	return f.makeContainer(ctx, container)
}

// makeContainer creates the container if it doesn't exist
func (f *Fs) makeContainer(ctx context.Context, container string) error {
	return f.cache.Create(container, func() error {
		// If this is a SAS URL limited to a container then assume it is already created
		if f.isLimited {
			return nil
		}
		opt := service.CreateContainerOptions{
			// Specifies whether data in the container may be accessed publicly and the level of access
			Access: &f.publicAccess,

			// Optional. Specifies a user-defined name-value pair associated with the blob.
			//Metadata map[string]string

			// Optional. Specifies the encryption scope settings to set on the container.
			//CpkScopeInfo *CpkScopeInfo
		}
		// now try to create the container
		return f.pacer.Call(func() (bool, error) {
			_, err := f.svc.CreateContainer(ctx, container, &opt)
			if err != nil {
				if storageErr, ok := err.(*azcore.ResponseError); ok {
					switch bloberror.Code(storageErr.ErrorCode) {
					case bloberror.ContainerAlreadyExists:
						return false, nil
					case bloberror.ContainerBeingDeleted:
						// From https://docs.microsoft.com/en-us/rest/api/storageservices/delete-container
						// When a container is deleted, a container with the same name cannot be created
						// for at least 30 seconds; the container may not be available for more than 30
						// seconds if the service is still processing the request.
						time.Sleep(6 * time.Second) // default 10 retries will be 60 seconds
						f.cache.MarkDeleted(container)
						return true, err
					}
				}
			}
			return f.shouldRetry(ctx, err)
		})
	}, nil)
}

// isEmpty checks to see if a given (container, directory) is empty and returns an error if not
func (f *Fs) isEmpty(ctx context.Context, containerName, directory string) (err error) {
	empty := true
	err = f.list(ctx, containerName, directory, f.rootDirectory, f.rootContainer == "", true, 1, func(remote string, object *container.BlobItem, isDirectory bool) error {
		empty = false
		return nil
	})
	if err != nil {
		return err
	}
	if !empty {
		return fs.ErrorDirectoryNotEmpty
	}
	return nil
}

// deleteContainer deletes the container.  It can delete a full
// container so use isEmpty if you don't want that.
func (f *Fs) deleteContainer(ctx context.Context, containerName string) error {
	return f.cache.Remove(containerName, func() error {
		getOptions := container.GetPropertiesOptions{}
		delOptions := container.DeleteOptions{}
		return f.pacer.Call(func() (bool, error) {
			_, err := f.cntSVC(containerName).GetProperties(ctx, &getOptions)
			if err == nil {
				_, err = f.cntSVC(containerName).Delete(ctx, &delOptions)
			}

			if err != nil {
				// Check http error code along with service code, current SDK doesn't populate service code correctly sometimes
				if storageErr, ok := err.(*azcore.ResponseError); ok && (storageErr.ErrorCode == string(bloberror.ContainerNotFound) || storageErr.StatusCode == http.StatusNotFound) {
					return false, fs.ErrorDirNotFound
				}

				return f.shouldRetry(ctx, err)
			}

			return f.shouldRetry(ctx, err)
		})
	})
}

// Rmdir deletes the container if the fs is at the root
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	container, directory := f.split(dir)
	if container == "" || directory != "" {
		return nil
	}
	err := f.isEmpty(ctx, container, directory)
	if err != nil {
		return err
	}
	return f.deleteContainer(ctx, container)
}

// Precision of the remote
func (f *Fs) Precision() time.Duration {
	return time.Nanosecond
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// Purge deletes all the files and directories including the old versions.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	container, directory := f.split(dir)
	if container == "" || directory != "" {
		// Delegate to caller if not root of a container
		return fs.ErrorCantPurge
	}
	return f.deleteContainer(ctx, container)
}

// Copy src to this remote using server-side copy operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	dstContainer, dstPath := f.split(remote)
	err := f.makeContainer(ctx, dstContainer)
	if err != nil {
		return nil, err
	}
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	dstBlobSVC := f.getBlobSVC(dstContainer, dstPath)
	srcBlobSVC := srcObj.getBlobSVC()
	srcURL := srcBlobSVC.URL()

	tier := blob.AccessTier(f.opt.AccessTier)
	options := blob.StartCopyFromURLOptions{
		Tier: &tier,
	}
	var startCopy blob.StartCopyFromURLResponse
	err = f.pacer.Call(func() (bool, error) {
		startCopy, err = dstBlobSVC.StartCopyFromURL(ctx, srcURL, &options)
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, err
	}

	copyStatus := startCopy.CopyStatus
	getOptions := blob.GetPropertiesOptions{}
	for copyStatus != nil && string(*copyStatus) == string(container.CopyStatusTypePending) {
		time.Sleep(1 * time.Second)
		getMetadata, err := dstBlobSVC.GetProperties(ctx, &getOptions)
		if err != nil {
			return nil, err
		}
		copyStatus = getMetadata.CopyStatus
	}

	return f.NewObject(ctx, remote)
}

func (f *Fs) getMemoryPool(size int64) *pool.Pool {
	if size == int64(f.opt.ChunkSize) {
		return f.pool
	}

	return pool.New(
		time.Duration(f.opt.MemoryPoolFlushTime),
		int(size),
		f.ci.Transfers,
		f.opt.MemoryPoolUseMmap,
	)
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

// Hash returns the MD5 of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	// Convert base64 encoded md5 into lower case hex
	if o.md5 == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(o.md5)
	if err != nil {
		return "", fmt.Errorf("failed to decode Content-MD5: %q: %w", o.md5, err)
	}
	return hex.EncodeToString(data), nil
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.size
}

func (o *Object) setMetadata(metadata map[string]string) {
	if len(metadata) > 0 {
		// Lower case the metadata
		o.meta = make(map[string]string, len(metadata))
		for k, v := range metadata {
			o.meta[strings.ToLower(k)] = v
		}
		if modTime, ok := o.meta[modTimeKey]; ok {
			when, err := time.Parse(timeFormatIn, modTime)
			if err != nil {
				fs.Debugf(o, "Couldn't parse %v = %q: %v", modTimeKey, modTime, err)
			}
			o.modTime = when
		}
	} else {
		o.meta = nil
	}
}

// Duplicte of setMetadata but taking pointers to strings
func (o *Object) setMetadataP(metadata map[string]*string) {
	if len(metadata) > 0 {
		// Convert the format of the metadata
		newMeta := make(map[string]string, len(metadata))
		for k, v := range metadata {
			if v != nil {
				newMeta[k] = *v
			}
		}
		o.setMetadata(newMeta)
	} else {
		o.meta = nil
	}
}

// decodeMetaDataFromPropertiesResponse sets the metadata from the data passed in
//
// Sets
//
//	o.id
//	o.modTime
//	o.size
//	o.md5
//	o.meta
func (o *Object) decodeMetaDataFromPropertiesResponse(info *blob.GetPropertiesResponse) (err error) {
	metadata := info.Metadata
	var size int64
	if info.ContentLength == nil {
		size = -1
	} else {
		size = *info.ContentLength
	}
	if isDirectoryMarker(size, metadata, o.remote) {
		return fs.ErrorNotAFile
	}
	// NOTE - Client library always returns MD5 as base64 decoded string, Object needs to maintain
	// this as base64 encoded string.
	o.md5 = base64.StdEncoding.EncodeToString(info.ContentMD5)
	if info.ContentType == nil {
		o.mimeType = ""
	} else {
		o.mimeType = *info.ContentType
	}
	o.size = size
	if info.LastModified == nil {
		o.modTime = time.Now()
	} else {
		o.modTime = *info.LastModified
	}
	if info.AccessTier == nil {
		o.accessTier = blob.AccessTier("")
	} else {
		o.accessTier = blob.AccessTier(*info.AccessTier)
	}
	o.setMetadata(metadata)

	return nil
}

func (o *Object) decodeMetaDataFromDownloadResponse(info *blob.DownloadStreamResponse) (err error) {
	metadata := info.Metadata
	var size int64
	if info.ContentLength == nil {
		size = -1
	} else {
		size = *info.ContentLength
	}
	if isDirectoryMarker(size, metadata, o.remote) {
		return fs.ErrorNotAFile
	}
	// NOTE - Client library always returns MD5 as base64 decoded string, Object needs to maintain
	// this as base64 encoded string.
	o.md5 = base64.StdEncoding.EncodeToString(info.ContentMD5)
	if info.ContentType == nil {
		o.mimeType = ""
	} else {
		o.mimeType = *info.ContentType
	}
	o.size = size
	if info.LastModified == nil {
		o.modTime = time.Now()
	} else {
		o.modTime = *info.LastModified
	}
	// FIXME response doesn't appear to have AccessTier in?
	// if info.AccessTier == nil {
	// 	o.accessTier = blob.AccessTier("")
	// } else {
	// 	o.accessTier = blob.AccessTier(*info.AccessTier)
	// }
	o.setMetadata(metadata)

	// If it was a Range request, the size is wrong, so correct it
	if info.ContentRange != nil {
		contentRange := *info.ContentRange
		slash := strings.IndexRune(contentRange, '/')
		if slash >= 0 {
			i, err := strconv.ParseInt(contentRange[slash+1:], 10, 64)
			if err == nil {
				o.size = i
			} else {
				fs.Debugf(o, "Failed to find parse integer from in %q: %v", contentRange, err)
			}
		} else {
			fs.Debugf(o, "Failed to find length in %q", contentRange)
		}
	}

	return nil
}

func (o *Object) decodeMetaDataFromBlob(info *container.BlobItem) (err error) {
	if info.Properties == nil {
		return errors.New("nil Properties in decodeMetaDataFromBlob")
	}
	metadata := info.Metadata
	var size int64
	if info.Properties.ContentLength == nil {
		size = -1
	} else {
		size = *info.Properties.ContentLength
	}
	if isDirectoryMarkerP(size, metadata, o.remote) {
		return fs.ErrorNotAFile
	}
	// NOTE - Client library always returns MD5 as base64 decoded string, Object needs to maintain
	// this as base64 encoded string.
	o.md5 = base64.StdEncoding.EncodeToString(info.Properties.ContentMD5)
	if info.Properties.ContentType == nil {
		o.mimeType = ""
	} else {
		o.mimeType = *info.Properties.ContentType
	}
	o.size = size
	if info.Properties.LastModified == nil {
		o.modTime = time.Now()
	} else {
		o.modTime = *info.Properties.LastModified
	}
	if info.Properties.AccessTier == nil {
		o.accessTier = blob.AccessTier("")
	} else {
		o.accessTier = *info.Properties.AccessTier
	}
	o.setMetadataP(metadata)

	return nil
}

// getBlobSVC creates a blob client
func (o *Object) getBlobSVC() *blob.Client {
	container, directory := o.split()
	return o.fs.getBlobSVC(container, directory)
}

// getBlockBlobSVC creates a block blob client
func (o *Object) getBlockBlobSVC() *blockblob.Client {
	container, directory := o.split()
	return o.fs.getBlockBlobSVC(container, directory)
}

// clearMetaData clears enough metadata so readMetaData will re-read it
func (o *Object) clearMetaData() {
	o.modTime = time.Time{}
}

// readMetaData gets the metadata if it hasn't already been fetched
//
// Sets
//
//	o.id
//	o.modTime
//	o.size
//	o.md5
func (o *Object) readMetaData() (err error) {
	container, _ := o.split()
	if !o.fs.containerOK(container) {
		return fs.ErrorObjectNotFound
	}
	if !o.modTime.IsZero() {
		return nil
	}
	blb := o.getBlobSVC()
	// fs.Debugf(o, "Blob URL = %q", blb.URL())

	// Read metadata (this includes metadata)
	options := blob.GetPropertiesOptions{}
	ctx := context.Background()
	var blobProperties blob.GetPropertiesResponse
	err = o.fs.pacer.Call(func() (bool, error) {
		blobProperties, err = blb.GetProperties(ctx, &options)
		return o.fs.shouldRetry(ctx, err)
	})
	if err != nil {
		// On directories - GetProperties does not work and current SDK does not populate service code correctly hence check regular http response as well
		if storageErr, ok := err.(*azcore.ResponseError); ok && (storageErr.ErrorCode == string(bloberror.BlobNotFound) || storageErr.StatusCode == http.StatusNotFound) {
			return fs.ErrorObjectNotFound
		}
		return err
	}

	return o.decodeMetaDataFromPropertiesResponse(&blobProperties)
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime(ctx context.Context) (result time.Time) {
	// The error is logged in readMetaData
	_ = o.readMetaData()
	return o.modTime
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	// Make sure o.meta is not nil
	if o.meta == nil {
		o.meta = make(map[string]string, 1)
	}
	// Set modTimeKey in it
	o.meta[modTimeKey] = modTime.Format(timeFormatOut)

	blb := o.getBlobSVC()
	opt := blob.SetMetadataOptions{}
	err := o.fs.pacer.Call(func() (bool, error) {
		_, err := blb.SetMetadata(ctx, o.meta, &opt)
		return o.fs.shouldRetry(ctx, err)
	})
	if err != nil {
		return err
	}
	o.modTime = modTime
	return nil
}

// Storable returns if this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	// Offset and Count for range download
	var offset int64
	var count int64
	if o.AccessTier() == blob.AccessTierArchive {
		return nil, fmt.Errorf("blob in archive tier, you need to set tier to hot or cool first")
	}
	fs.FixRangeOption(options, o.size)
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			offset, count = x.Decode(o.size)
			if count < 0 {
				count = o.size - offset
			}
		case *fs.SeekOption:
			offset = x.Offset
		default:
			if option.Mandatory() {
				fs.Logf(o, "Unsupported mandatory option: %v", option)
			}
		}
	}
	blb := o.getBlobSVC()
	opt := blob.DownloadStreamOptions{
		// When set to true and specified together with the Range, the service returns the MD5 hash for the range, as long as the
		// range is less than or equal to 4 MB in size.
		//RangeGetContentMD5 *bool

		// Range specifies a range of bytes.  The default value is all bytes.
		//Range HTTPRange
		Range: blob.HTTPRange{
			Offset: offset,
			Count:  count,
		},

		// AccessConditions *AccessConditions
		// CpkInfo          *CpkInfo
		// CpkScopeInfo     *CpkScopeInfo
	}
	var downloadResponse blob.DownloadStreamResponse
	err = o.fs.pacer.Call(func() (bool, error) {
		downloadResponse, err = blb.DownloadStream(ctx, &opt)
		return o.fs.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open for download: %w", err)
	}
	err = o.decodeMetaDataFromDownloadResponse(&downloadResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to decode metadata for download: %w", err)
	}
	return downloadResponse.Body, nil
}

// dontEncode is the characters that do not need percent-encoding
//
// The characters that do not need percent-encoding are a subset of
// the printable ASCII characters: upper-case letters, lower-case
// letters, digits, ".", "_", "-", "/", "~", "!", "$", "'", "(", ")",
// "*", ";", "=", ":", and "@". All other byte values in a UTF-8 must
// be replaced with "%" and the two-digit hex value of the byte.
const dontEncode = (`abcdefghijklmnopqrstuvwxyz` +
	`ABCDEFGHIJKLMNOPQRSTUVWXYZ` +
	`0123456789` +
	`._-/~!$'()*;=:@`)

// noNeedToEncode is a bitmap of characters which don't need % encoding
var noNeedToEncode [256]bool

func init() {
	for _, c := range dontEncode {
		noNeedToEncode[c] = true
	}
}

// urlEncode encodes in with % encoding
func urlEncode(in string) string {
	var out bytes.Buffer
	for i := 0; i < len(in); i++ {
		c := in[i]
		if noNeedToEncode[c] {
			_ = out.WriteByte(c)
		} else {
			_, _ = out.WriteString(fmt.Sprintf("%%%2X", c))
		}
	}
	return out.String()
}

// poolWrapper wraps a pool.Pool as an azblob.TransferManager
type poolWrapper struct {
	pool     *pool.Pool
	bufToken chan struct{}
	runToken chan struct{}
}

// newPoolWrapper creates an azblob.TransferManager that will use a
// pool.Pool with maximum concurrency as specified.
func (f *Fs) newPoolWrapper(concurrency int) *poolWrapper {
	return &poolWrapper{
		pool:     f.pool,
		bufToken: make(chan struct{}, concurrency),
		runToken: make(chan struct{}, concurrency),
	}
}

// Get implements TransferManager.Get().
func (pw *poolWrapper) Get() []byte {
	pw.bufToken <- struct{}{}
	return pw.pool.Get()
}

// Put implements TransferManager.Put().
func (pw *poolWrapper) Put(b []byte) {
	pw.pool.Put(b)
	<-pw.bufToken
}

// Run implements TransferManager.Run().
func (pw *poolWrapper) Run(f func()) {
	pw.runToken <- struct{}{}
	go func() {
		f()
		<-pw.runToken
	}()
}

// Close implements TransferManager.Close().
func (pw *poolWrapper) Close() {
}

// Converts a string into a pointer to a string
func pString(s string) *string {
	return &s
}

// readSeekCloser joins an io.Reader and an io.Seeker and provides a no-op io.Closer
type readSeekCloser struct {
	io.Reader
	io.Seeker
}

// Close does nothing
func (rs *readSeekCloser) Close() error {
	return nil
}

// increment the slice passed in as LSB binary
func increment(xs []byte) {
	for i, digit := range xs {
		newDigit := digit + 1
		xs[i] = newDigit
		if newDigit >= digit {
			// exit if no carry
			break
		}
	}
}

var warnStreamUpload sync.Once

// uploadMultipart uploads a file using multipart upload
//
// Write a larger blob, using CreateBlockBlob, PutBlock, and PutBlockList.
func (o *Object) uploadMultipart(ctx context.Context, in io.Reader, size int64, blb *blockblob.Client, httpHeaders *blob.HTTPHeaders) (err error) {
	// Calculate correct partSize
	partSize := o.fs.opt.ChunkSize
	totalParts := -1

	// make concurrency machinery
	concurrency := o.fs.opt.UploadConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	tokens := pacer.NewTokenDispenser(concurrency)

	// Note that the max size of file is 4.75 TB (100 MB X 50,000
	// blocks) and this is bigger than the max uncommitted block
	// size (9.52 TB) so we do not need to part commit block lists
	// or garbage collect uncommitted blocks.
	//
	// See: https://docs.microsoft.com/en-gb/rest/api/storageservices/put-block

	// size can be -1 here meaning we don't know the size of the incoming file.  We use ChunkSize
	// buffers here (default 4MB). With a maximum number of parts (50,000) this will be a file of
	// 195GB which seems like a not too unreasonable limit.
	if size == -1 {
		warnStreamUpload.Do(func() {
			fs.Logf(o, "Streaming uploads using chunk size %v will have maximum file size of %v",
				o.fs.opt.ChunkSize, partSize*fs.SizeSuffix(blockblob.MaxBlocks))
		})
	} else {
		partSize = chunksize.Calculator(o, size, blockblob.MaxBlocks, o.fs.opt.ChunkSize)
		if partSize > fs.SizeSuffix(blockblob.MaxStageBlockBytes) {
			return fmt.Errorf("can't upload as it is too big %v - takes more than %d chunks of %v", fs.SizeSuffix(size), fs.SizeSuffix(blockblob.MaxBlocks), fs.SizeSuffix(blockblob.MaxStageBlockBytes))
		}
		totalParts = int(fs.SizeSuffix(size) / partSize)
		if fs.SizeSuffix(size)%partSize != 0 {
			totalParts++
		}
	}

	fs.Debugf(o, "Multipart upload session started for %d parts of size %v", totalParts, partSize)

	// unwrap the accounting from the input, we use wrap to put it
	// back on after the buffering
	in, wrap := accounting.UnWrap(in)

	// FIXME it would be nice to delete uncommitted blocks
	// See: https://github.com/rclone/rclone/issues/5583
	//
	// However there doesn't seem to be an easy way of doing this other than
	// by deleting the target.
	//
	// This means that a failed upload deletes the target which isn't ideal.
	//
	// Uploading a zero length blob and deleting it will remove the
	// uncommitted blocks I think.
	//
	// Could check to see if a file exists already and if it
	// doesn't then create a 0 length file and delete it to flush
	// the uncommitted blocks.
	//
	// This is what azcopy does
	// https://github.com/MicrosoftDocs/azure-docs/issues/36347#issuecomment-541457962
	// defer atexit.OnError(&err, func() {
	// 	fs.Debugf(o, "Cancelling multipart upload")
	//      // Code goes here!
	// })()

	// Upload the chunks
	var (
		g, gCtx       = errgroup.WithContext(ctx)
		remaining     = fs.SizeSuffix(size)                 // remaining size in file for logging only, -1 if size < 0
		position      = fs.SizeSuffix(0)                    // position in file
		memPool       = o.fs.getMemoryPool(int64(partSize)) // pool to get memory from
		finished      = false                               // set when we have read EOF
		blocks        []string                              // list of blocks for finalize
		binaryBlockID = make([]byte, 8)                     // block counter as LSB first 8 bytes
	)
	for part := 0; !finished; part++ {
		// Get a block of memory from the pool and a token which limits concurrency
		tokens.Get()
		buf := memPool.Get()

		free := func() {
			memPool.Put(buf) // return the buf
			tokens.Put()     // return the token
		}

		// Fail fast, in case an errgroup managed function returns an error
		// gCtx is cancelled. There is no point in uploading all the other parts.
		if gCtx.Err() != nil {
			free()
			break
		}

		// Read the chunk
		n, err := readers.ReadFill(in, buf) // this can never return 0, nil
		if err == io.EOF {
			if n == 0 { // end if no data
				free()
				break
			}
			finished = true
		} else if err != nil {
			free()
			return fmt.Errorf("multipart upload failed to read source: %w", err)
		}
		buf = buf[:n]

		// increment the blockID and save the blocks for finalize
		increment(binaryBlockID)
		blockID := base64.StdEncoding.EncodeToString(binaryBlockID)
		blocks = append(blocks, blockID)

		// Transfer the chunk
		fs.Debugf(o, "Uploading part %d/%d offset %v/%v part size %d", part+1, totalParts, position, fs.SizeSuffix(size), len(buf))
		g.Go(func() (err error) {
			defer free()

			// Upload the block, with MD5 for check
			md5sum := md5.Sum(buf)
			transactionalMD5 := md5sum[:]
			err = o.fs.pacer.Call(func() (bool, error) {
				bufferReader := bytes.NewReader(buf)
				wrappedReader := wrap(bufferReader)
				rs := readSeekCloser{wrappedReader, bufferReader}
				options := blockblob.StageBlockOptions{
					// Specify the transactional md5 for the body, to be validated by the service.
					TransactionalContentMD5: transactionalMD5,
				}
				_, err = blb.StageBlock(ctx, blockID, &rs, &options)
				return o.fs.shouldRetry(ctx, err)
			})
			if err != nil {
				return fmt.Errorf("multipart upload failed to upload part: %w", err)
			}
			return nil
		})

		// ready for next block
		if size >= 0 {
			remaining -= partSize
		}
		position += partSize
	}
	err = g.Wait()
	if err != nil {
		return err
	}

	tier := blob.AccessTier(o.fs.opt.AccessTier)
	options := blockblob.CommitBlockListOptions{
		Metadata:    o.meta,
		Tier:        &tier,
		HTTPHeaders: httpHeaders,
	}

	// Finalise the upload session
	err = o.fs.pacer.Call(func() (bool, error) {
		_, err := blb.CommitBlockList(ctx, blocks, &options)
		return o.fs.shouldRetry(ctx, err)
	})
	if err != nil {
		return fmt.Errorf("multipart upload failed to finalize: %w", err)
	}
	return nil
}

// uploadSinglepart uploads a short blob using a single part upload
func (o *Object) uploadSinglepart(ctx context.Context, in io.Reader, size int64, blb *blockblob.Client, httpHeaders *blob.HTTPHeaders) (err error) {
	// fs.Debugf(o, "Single part upload starting of object %d bytes", size)
	if size > o.fs.poolSize || size < 0 {
		return fmt.Errorf("internal error: single part upload size too big %d > %d", size, o.fs.opt.ChunkSize)
	}

	buf := o.fs.pool.Get()
	defer o.fs.pool.Put(buf)

	n, err := readers.ReadFill(in, buf)
	if err == nil {
		// Check to see whether in is exactly len(buf) or bigger
		var buf2 = []byte{0}
		n2, err2 := readers.ReadFill(in, buf2)
		if n2 != 0 || err2 != io.EOF {
			return fmt.Errorf("single part upload read failed: object longer than expected (expecting %d but got > %d)", size, len(buf))
		}
	}
	if err != nil && err != io.EOF {
		return fmt.Errorf("single part upload read failed: %w", err)
	}
	if int64(n) != size {
		return fmt.Errorf("single part upload: expecting to read %d bytes but read %d", size, n)
	}

	b := bytes.NewReader(buf[:n])
	rs := &readSeekCloser{Reader: b, Seeker: b}

	tier := blob.AccessTier(o.fs.opt.AccessTier)
	options := blockblob.UploadOptions{
		Metadata:    o.meta,
		Tier:        &tier,
		HTTPHeaders: httpHeaders,
	}

	// Don't retry, return a retry error instead
	return o.fs.pacer.CallNoRetry(func() (bool, error) {
		_, err = blb.Upload(ctx, rs, &options)
		return o.fs.shouldRetry(ctx, err)
	})
}

// Update the object with the contents of the io.Reader, modTime and size
//
// The new object may have been created if an error is returned
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	if o.accessTier == blob.AccessTierArchive {
		if o.fs.opt.ArchiveTierDelete {
			fs.Debugf(o, "deleting archive tier blob before updating")
			err = o.Remove(ctx)
			if err != nil {
				return fmt.Errorf("failed to delete archive blob before updating: %w", err)
			}
		} else {
			return errCantUpdateArchiveTierBlobs
		}
	}
	container, containerPath := o.split()
	if container == "" || containerPath == "" {
		return fmt.Errorf("can't upload to root - need a container")
	}
	err = o.fs.makeContainer(ctx, container)
	if err != nil {
		return err
	}

	// Update Mod time
	o.updateMetadataWithModTime(src.ModTime(ctx))
	if err != nil {
		return err
	}

	// Create the HTTP headers for the upload
	httpHeaders := blob.HTTPHeaders{
		BlobContentType: pString(fs.MimeType(ctx, src)),
	}

	// Compute the Content-MD5 of the file. As we stream all uploads it
	// will be set in PutBlockList API call using the 'x-ms-blob-content-md5' header
	if !o.fs.opt.DisableCheckSum {
		if sourceMD5, _ := src.Hash(ctx, hash.MD5); sourceMD5 != "" {
			sourceMD5bytes, err := hex.DecodeString(sourceMD5)
			if err == nil {
				httpHeaders.BlobContentMD5 = sourceMD5bytes
			} else {
				fs.Debugf(o, "Failed to decode %q as MD5: %v", sourceMD5, err)
			}
		}
	}

	// Apply upload options (also allows one to overwrite content-type)
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "":
			// ignore
		case "cache-control":
			httpHeaders.BlobCacheControl = pString(value)
		case "content-disposition":
			httpHeaders.BlobContentDisposition = pString(value)
		case "content-encoding":
			httpHeaders.BlobContentEncoding = pString(value)
		case "content-language":
			httpHeaders.BlobContentLanguage = pString(value)
		case "content-type":
			httpHeaders.BlobContentType = pString(value)
		}
	}

	blb := o.fs.getBlockBlobSVC(container, containerPath)
	size := src.Size()
	multipartUpload := size < 0 || size > o.fs.poolSize

	if multipartUpload {
		err = o.uploadMultipart(ctx, in, size, blb, &httpHeaders)
	} else {
		err = o.uploadSinglepart(ctx, in, size, blb, &httpHeaders)
	}
	if err != nil {
		return err
	}

	// Refresh metadata on object
	o.clearMetaData()
	err = o.readMetaData()
	if err != nil {
		return err
	}

	// If tier is not changed or not specified, do not attempt to invoke `SetBlobTier` operation
	if o.fs.opt.AccessTier == string(defaultAccessTier) || o.fs.opt.AccessTier == string(o.AccessTier()) {
		return nil
	}

	// Now, set blob tier based on configured access tier
	return o.SetTier(o.fs.opt.AccessTier)
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	blb := o.getBlobSVC()
	//only := blob.DeleteSnapshotsOptionTypeOnly
	opt := blob.DeleteOptions{
		//DeleteSnapshots: &only,
	}
	return o.fs.pacer.Call(func() (bool, error) {
		_, err := blb.Delete(ctx, &opt)
		return o.fs.shouldRetry(ctx, err)
	})
}

// MimeType of an Object if known, "" otherwise
func (o *Object) MimeType(ctx context.Context) string {
	return o.mimeType
}

// AccessTier of an object, default is of type none
func (o *Object) AccessTier() blob.AccessTier {
	return o.accessTier
}

// SetTier performs changing object tier
func (o *Object) SetTier(tier string) error {
	if !validateAccessTier(tier) {
		return fmt.Errorf("tier %s not supported by Azure Blob Storage", tier)
	}

	// Check if current tier already matches with desired tier
	if o.GetTier() == tier {
		return nil
	}
	desiredAccessTier := blob.AccessTier(tier)
	blb := o.getBlobSVC()
	ctx := context.Background()
	priority := blob.RehydratePriorityStandard
	opt := blob.SetTierOptions{
		RehydratePriority: &priority,
	}
	err := o.fs.pacer.Call(func() (bool, error) {
		_, err := blb.SetTier(ctx, desiredAccessTier, &opt)
		return o.fs.shouldRetry(ctx, err)
	})

	if err != nil {
		return fmt.Errorf("failed to set Blob Tier: %w", err)
	}

	// Set access tier on local object also, this typically
	// gets updated on get blob properties
	o.accessTier = desiredAccessTier
	fs.Debugf(o, "Successfully changed object tier to %s", tier)

	return nil
}

// GetTier returns object tier in azure as string
func (o *Object) GetTier() string {
	return string(o.accessTier)
}

// Check the interfaces are satisfied
var (
	_ fs.Fs          = &Fs{}
	_ fs.Copier      = &Fs{}
	_ fs.PutStreamer = &Fs{}
	_ fs.Purger      = &Fs{}
	_ fs.ListRer     = &Fs{}
	_ fs.Object      = &Object{}
	_ fs.MimeTyper   = &Object{}
	_ fs.GetTierer   = &Object{}
	_ fs.SetTierer   = &Object{}
)
