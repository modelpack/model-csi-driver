package service

import (
	"encoding/json"
	"os"

	"github.com/pkg/errors"
)

// LayerMetadataEntry stores the digest and file path for a single layer,
// persisted alongside the volume so the LayerCache can be rebuilt on restart.
type LayerMetadataEntry struct {
	Digest   string `json:"digest"`
	FilePath string `json:"file_path"`
	Size     int64  `json:"size"`
}

// saveLayerMetadata writes the layer metadata to the given path.
func saveLayerMetadata(path string, entries []LayerMetadataEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return errors.Wrap(err, "marshal layer metadata")
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return errors.Wrap(err, "write temp layer metadata")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // clean up on failure
		return errors.Wrap(err, "rename layer metadata")
	}
	return nil
}

// loadLayerMetadata reads the layer metadata from the given path.
func loadLayerMetadata(path string) ([]LayerMetadataEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "read layer metadata")
	}
	var entries []LayerMetadataEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, errors.Wrap(err, "unmarshal layer metadata")
	}
	return entries, nil
}
