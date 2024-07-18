package server

import (
	"net/http"
	"time"

	"github.com/openfluxcd/controller-manager/pkg/storage"
)

// InitServer starts a file server to serve artifacts.
func InitServer(path, storageAddress string, artifactRetentionTTL time.Duration, artifactRetentionRecords int) error {
	stg, err := storage.NewStorage(path, storageAddress, artifactRetentionTTL, artifactRetentionRecords)
	if err != nil {
		return err
	}

	return startFileServer(stg.BasePath, storageAddress)
}

func startFileServer(path string, address string) error {
	fs := http.FileServer(http.Dir(path))
	mux := http.NewServeMux()
	mux.Handle("/", fs)
	return http.ListenAndServe(address, mux)
}
