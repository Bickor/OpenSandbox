package katavmstate

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/require"
)

func TestDelegatedBlobSASValidation(t *testing.T) {
	now := time.Now().UTC()
	valid := url.Values{"skoid": {"id"}, "sktid": {"tenant"}, "skt": {"start"}, "ske": {"end"}, "sks": {"b"}, "skv": {"version"}, "sig": {"secret"}, "se": {now.Add(time.Hour).Format(time.RFC3339)}, "sr": {"c"}, "spr": {"https"}}
	_, err := newDelegatedBlobStore("https://example.blob.core.windows.net", "test", valid.Encode(), now)
	require.NoError(t, err)
	minutePrecision, _ := url.ParseQuery(valid.Encode())
	minutePrecision.Set("se", now.Add(time.Hour).Format("2006-01-02T15:04Z"))
	_, err = newDelegatedBlobStore("https://example.blob.core.windows.net", "test", minutePrecision.Encode(), now)
	require.NoError(t, err)
	for _, test := range []struct{ key, value string }{{"skoid", ""}, {"sr", "b"}, {"spr", "https,http"}, {"se", now.Add(-time.Hour).Format(time.RFC3339)}, {"se", now.Add(48 * time.Hour).Format(time.RFC3339)}, {"ss", "b"}} {
		copy, _ := url.ParseQuery(valid.Encode())
		copy.Set(test.key, test.value)
		_, err := newDelegatedBlobStore("https://example.blob.core.windows.net", "test", copy.Encode(), now)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}

func TestDelegatedBlobErrorsNeverExposeCredential(t *testing.T) {
	store := &blobStore{delegatedSAS: true}
	err := store.transferError(errors.New("GET https://account/?sig=SECRET failed"))
	require.NotContains(t, err.Error(), "SECRET")
	err = store.transferError(&azcore.ResponseError{StatusCode: 403, ErrorCode: "AuthorizationPermissionMismatch"})
	require.Contains(t, err.Error(), "403")
}
