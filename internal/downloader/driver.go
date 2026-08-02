package downloader

import (
	"context"
	"fmt"
	"time"
)

type Credential struct {
	username string
	password string
}

func NewCredential(username, password string) (Credential, error) {
	if password == "" {
		return Credential{}, fmt.Errorf("empty downloader password")
	}
	if len(password) > 64*1024 {
		return Credential{}, fmt.Errorf("downloader password exceeds 64 KiB")
	}
	return Credential{username: username, password: password}, nil
}

func (c Credential) UsernameValue() string { return c.username }
func (c Credential) PasswordValue() string { return c.password }
func (c Credential) String() string        { return "[REDACTED]" }
func (c Credential) GoString() string      { return "downloader.Credential{[REDACTED]}" }

type Status struct {
	Driver        string    `json:"driver"`
	Endpoint      string    `json:"endpoint"`
	Version       string    `json:"version"`
	WebAPIVersion string    `json:"web_api_version,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

type Torrent struct {
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	SizeBytes  int64   `json:"size_bytes"`
	Progress   float64 `json:"progress"`
	State      string  `json:"state"`
	SavePath   string  `json:"save_path"`
	Downloaded int64   `json:"downloaded_bytes"`
	Uploaded   int64   `json:"uploaded_bytes"`
}

type Driver interface {
	Status(context.Context, Credential) (Status, error)
	Torrents(context.Context, Credential) ([]Torrent, error)
}
