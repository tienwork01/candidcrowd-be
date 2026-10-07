package media

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestClosedUploadWindowRefusesNewUploadsButNotRetries(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true}
	reserved := CreateInput{Filename: "a.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ClientUploadID: uuid.New()}
	_, err := service.CreateUpload(context.Background(), scope, reserved)
	require.NoError(t, err)

	scope.UploadClosed = true
	_, err = service.CreateUpload(context.Background(), scope, reserved)
	require.NoError(t, err, "an upload reserved before the window closed can still get its target")

	fresh := reserved
	fresh.ClientUploadID = uuid.New()
	fresh.ChecksumSHA256 = "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, err = service.CreateUpload(context.Background(), scope, fresh)
	require.ErrorIs(t, err, ErrUploadClosed)
}

func TestQuotaErrorsDescribeThemselves(t *testing.T) {
	var coder apierror.Coder
	err := error(&QuotaError{Resource: ResourcePhoto, Limit: 30, Usage: 30})
	require.True(t, errors.As(err, &coder))
	api := coder.APIError()
	require.Equal(t, http.StatusUnprocessableEntity, api.Status)
	require.Equal(t, "event_quota_exceeded", api.Code)
	require.Equal(t, map[string]any{"resource": "photo", "limit": int64(30), "usage": int64(30)}, api.Details)

	require.Contains(t, (&QuotaError{Resource: ResourceBytes}).APIError().Message, "storage")

	require.True(t, errors.As(ErrUploadClosed, &coder))
	require.Equal(t, "event_upload_closed", coder.APIError().Code)
}

func TestMediaKind(t *testing.T) {
	p, v := MediaKind("image/heic")
	require.Equal(t, [2]int{1, 0}, [2]int{p, v})
	p, v = MediaKind("video/quicktime")
	require.Equal(t, [2]int{0, 1}, [2]int{p, v})
}
