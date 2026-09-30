package katavmstate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

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
	client    *azblob.Client
	container string
}

func newBlobStore(account, container string) (objectStore, error) {
	u, err := url.Parse(account)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Blob account must be an HTTPS account URL without credentials, query, or path")
	}
	if container == "" || strings.ContainsAny(container, "/\\?#") {
		return nil, errors.New("Blob container is required and must be a single name")
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
	return err
}

func (s *blobStore) get(ctx context.Context, key string) (io.ReadCloser, error) {
	response, err := s.client.DownloadStream(ctx, s.container, key, nil)
	if err != nil {
		return nil, err
	}
	return response.NewRetryReader(ctx, nil), nil
}

func (s *blobStore) deletePrefix(ctx context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("invalid deletion prefix")
	}
	pager := s.client.NewListBlobsFlatPager(s.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, item := range page.Segment.BlobItems {
			_, err := s.client.DeleteBlob(ctx, s.container, *item.Name, nil)
			if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
				return err
			}
		}
	}
	return nil
}
