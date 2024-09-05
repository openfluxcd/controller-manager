package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openfluxcd/controller-manager/storage"
)

// NewStorage creates a storage and returns the means to launch a file server to serve created Artifacts.
func NewStorage(c client.Client, scheme *runtime.Scheme, path, storageAddress string, artifactRetentionTTL time.Duration, artifactRetentionRecords int) (*storage.Storage, error) {
	stg, err := storage.NewStorage(c, scheme, path, storageAddress, artifactRetentionTTL, artifactRetentionRecords)
	if err != nil {
		return nil, fmt.Errorf("error initializing storage: %v", err)
	}

	return stg, nil
}

type ArtifactServer struct {
	server  *http.Server
	timeout time.Duration
}

func NewArtifactServer(path string, address string, timeout time.Duration) (*ArtifactServer, error) {
	fs := http.FileServer(http.Dir(path))
	mux := http.NewServeMux()
	mux.Handle("/", fs)

	s := &http.Server{
		Addr:    address,
		Handler: mux,
	}
	as := &ArtifactServer{
		server:  s,
		timeout: timeout,
	}
	return as, nil
}

func (s *ArtifactServer) Start(ctx context.Context) error {
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- s.server.ListenAndServe()
	}()
	var err error
	var cancel context.CancelFunc
	select {
	case <-ctx.Done():
		ctx, cancel = context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		err = s.server.Shutdown(ctx)
	case err = <-serverErr:
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
