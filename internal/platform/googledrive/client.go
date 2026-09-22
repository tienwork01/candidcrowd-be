// Package googledrive is the infrastructure adapter for the owner's private
// Google Drive archive. It never exposes Drive file IDs or share links to API clients.
package googledrive

import (
	"context"
	"io"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type Client struct {
	service      *drive.Service
	rootFolderID string
}

func New(ctx context.Context, clientID, clientSecret, refreshToken, rootFolderID string) (*Client, error) {
	config := &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: oauth2.Endpoint{TokenURL: "https://oauth2.googleapis.com/token"}}
	tokenSource := config.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	service, err := drive.NewService(ctx, option.WithTokenSource(tokenSource), option.WithScopes(drive.DriveFileScope))
	if err != nil {
		return nil, err
	}
	return &Client{service: service, rootFolderID: rootFolderID}, nil
}

// Upload stores an archive object privately and returns only its provider-local ID.
func (c *Client) Upload(ctx context.Context, name, mimeType string, body io.Reader) (string, error) {
	file := &drive.File{Name: name, Parents: []string{c.rootFolderID}, MimeType: mimeType}
	created, err := c.service.Files.Create(file).Media(body).Context(ctx).Fields("id").Do()
	if err != nil {
		return "", err
	}
	return created.Id, nil
}

func (c *Client) Delete(ctx context.Context, fileID string) error {
	return c.service.Files.Delete(fileID).Context(ctx).Do()
}

func (c *Client) OpenRead(ctx context.Context, fileID string) (io.ReadCloser, error) {
	response, err := c.service.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}
