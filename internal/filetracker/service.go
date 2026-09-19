// Package filetracker provides functionality to track file reads in sessions.
package filetracker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// Service defines the interface for tracking file reads in sessions.
type Service interface {
	// RecordRead records when a file was read. Content is the served
	// file content used to anchor the read: its hash and line count
	// are stored alongside the read. An empty content only refreshes
	// the read timestamp and preserves any stored anchor.
	RecordRead(ctx context.Context, sessionID, path, content string)

	// LastReadTime returns when a file was last read.
	// Returns zero time if never read.
	LastReadTime(ctx context.Context, sessionID, path string) time.Time

	// ListReadFiles returns the paths of all files read in a session.
	ListReadFiles(ctx context.Context, sessionID string) ([]string, error)

	// LatestAnchor returns the most recent anchored read of a
	// session: the absolute path, the first 8 hex chars of the
	// served content hash, and its line count. It reports false
	// when the session has no read with stored anchor content.
	LatestAnchor(ctx context.Context, sessionID string) (ReadAnchor, bool)
}

// ReadAnchor is one session's latest content-anchored file read.
type ReadAnchor struct {
	Path string
	SHA8 string
	// Lines is the line count of the anchored revision.
	Lines int
}

type service struct {
	q *db.Queries
}

// NewService creates a new file tracker service.
func NewService(q *db.Queries) Service {
	return &service{q: q}
}

// HashContent anchors content: the first 8 hex chars of its SHA-256
// and its line count. Both producers (view tool records) and
// consumers (delivery stale checks) must use this one function so
// the comparison is over identical inputs.
func HashContent(content string) (sha8 string, lines int) {
	sum := sha256.Sum256([]byte(content))
	sha8 = hex.EncodeToString(sum[:])[:8]
	if content == "" {
		return sha8, 0
	}
	lines = strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return sha8, lines
}

// RecordRead records when a file was read.
func (s *service) RecordRead(ctx context.Context, sessionID, path, content string) {
	params := db.RecordFileReadParams{
		SessionID: sessionID,
		Path:      relpath(path),
	}
	if content != "" {
		sha8, lines := HashContent(content)
		params.ContentSha256_8 = sql.NullString{String: sha8, Valid: true}
		params.Lines = sql.NullInt64{Int64: int64(lines), Valid: true}
	} else if existing, err := s.q.GetFileRead(ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      relpath(path),
	}); err == nil {
		// A content-less record (an edit or rename mark) must not
		// wipe a stored anchor for the same path.
		params.ContentSha256_8 = existing.ContentSha256_8
		params.Lines = existing.Lines
	}
	if err := s.q.RecordFileRead(ctx, params); err != nil {
		slog.Error("Error recording file read", "error", err, "file", path)
	}
}

// LatestAnchor returns the most recent anchored read of a session.
func (s *service) LatestAnchor(ctx context.Context, sessionID string) (ReadAnchor, bool) {
	readFiles, err := s.q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return ReadAnchor{}, false
	}
	basepath, err := os.Getwd()
	if err != nil {
		return ReadAnchor{}, false
	}
	for _, rf := range readFiles {
		if !rf.ContentSha256_8.Valid {
			continue
		}
		anchor := ReadAnchor{
			Path: filepath.Join(basepath, rf.Path),
			SHA8: rf.ContentSha256_8.String,
		}
		if rf.Lines.Valid {
			anchor.Lines = int(rf.Lines.Int64)
		}
		return anchor, true
	}
	return ReadAnchor{}, false
}

// LastReadTime returns when a file was last read.
// Returns zero time if never read.
func (s *service) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	readFile, err := s.q.GetFileRead(ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      relpath(path),
	})
	if err != nil {
		return time.Time{}
	}

	return time.Unix(readFile.ReadAt, 0)
}

func relpath(path string) string {
	path = filepath.Clean(path)
	basepath, err := os.Getwd()
	if err != nil {
		slog.Warn("Error getting basepath", "error", err)
		return path
	}
	relpath, err := filepath.Rel(basepath, path)
	if err != nil {
		slog.Warn("Error getting relpath", "error", err)
		return path
	}
	return relpath
}

// ListReadFiles returns the paths of all files read in a session.
func (s *service) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	readFiles, err := s.q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing read files: %w", err)
	}

	basepath, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting working directory: %w", err)
	}

	paths := make([]string, 0, len(readFiles))
	for _, rf := range readFiles {
		paths = append(paths, filepath.Join(basepath, rf.Path))
	}
	return paths, nil
}
