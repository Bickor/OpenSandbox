package katavmstate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// objectStore intentionally accepts keys, not caller-supplied download URLs.
type objectStore interface {
	put(context.Context, string, io.Reader) error
	get(context.Context, string) (io.ReadCloser, error)
	deletePrefix(context.Context, string) error
}

type blobStore struct {
	client       *azblob.Client
	container    string
	delegatedSAS bool
}

func newBlobStore(account, container string) (objectStore, error) {
	u, err := url.Parse(account)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Blob account must be an HTTPS account URL without credentials, query, or path")
	}
	if container == "" || strings.ContainsAny(container, "/\\?#") {
		return nil, errors.New("Blob container is required and must be a single name")
	}
	if token := os.Getenv("KATA_SNAPSHOT_BLOB_SAS_TOKEN"); token != "" {
		return newDelegatedBlobStore(account, container, token, time.Now())
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClient(account, credential, nil)
	if err != nil {
		return nil, err
	}
	return &blobStore{client: client, container: container}, nil
}

// Optional test/BYOC credential path. Only short-lived, HTTPS, container-scoped
// user-delegation SAS is accepted; account keys and account SAS are not supported.
// Inject through a Kubernetes Secret, never a controller flag or snapshot status.
func newDelegatedBlobStore(account, container, token string, now time.Time) (objectStore, error) {
	values, err := url.ParseQuery(strings.TrimPrefix(strings.TrimSpace(token), "?"))
	if err != nil {
		return nil, errors.New("invalid delegated Blob SAS")
	}
	for _, key := range []string{"skoid", "sktid", "skt", "ske", "sks", "skv", "sig", "se"} {
		if values.Get(key) == "" || len(values[key]) != 1 {
			return nil, errors.New("container user-delegation SAS is required")
		}
	}
	expires, err := time.Parse(time.RFC3339, values.Get("se"))
	if err != nil || !expires.After(now) || expires.After(now.Add(24*time.Hour)) || values.Get("sr") != "c" || values.Get("spr") != "https" || values.Get("ss") != "" || values.Get("srt") != "" {
		return nil, errors.New("delegated SAS must be HTTPS, container-scoped, and expire within 24 hours")
	}
	client, err := azblob.NewClientWithNoCredential(strings.TrimRight(account, "/")+"?"+values.Encode(), nil)
	if err != nil {
		return nil, errors.New("cannot configure delegated Blob client")
	}
	return &blobStore{client: client, container: container, delegatedSAS: true}, nil
}

func (s *blobStore) transferError(err error) error {
	if err == nil || !s.delegatedSAS {
		return err
	}
	// Azure SDK errors can contain the request URL, including its SAS query.
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("delegated Blob request failed: HTTP %d (%s)", response.StatusCode, response.ErrorCode)
	}
	return errors.New("delegated Blob request failed; check connectivity and SAS validity")
}

// Objects are immutable. A retry may encounter a previously committed object;
// callers verify the committed manifest instead of overwriting it.
func (s *blobStore) put(ctx context.Context, key string, body io.Reader) error {
	any := azcore.ETag("*")
	_, err := s.client.UploadStream(ctx, s.container, key, body, &azblob.UploadStreamOptions{
		BlockSize: 4 * 1024 * 1024, Concurrency: 2,
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &any}},
	})
	if bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet) {
		return nil
	}
	return s.transferError(err)
}

func (s *blobStore) get(ctx context.Context, key string) (io.ReadCloser, error) {
	response, err := s.client.DownloadStream(ctx, s.container, key, nil)
	if err != nil {
		return nil, s.transferError(err)
	}
	reader := response.NewRetryReader(ctx, nil)
	if s.delegatedSAS {
		return &redactedBlobReader{ReadCloser: reader, store: s}, nil
	}
	return reader, nil
}

type redactedBlobReader struct {
	io.ReadCloser
	store *blobStore
}

func (r *redactedBlobReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		return n, err
	}
	return n, r.store.transferError(err)
}

func (s *blobStore) deletePrefix(ctx context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("invalid deletion prefix")
	}
	pager := s.client.NewListBlobsFlatPager(s.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return s.transferError(err)
		}
		for _, item := range page.Segment.BlobItems {
			_, err := s.client.DeleteBlob(ctx, s.container, *item.Name, nil)
			if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
				return s.transferError(err)
			}
		}
	}
	return nil
}
