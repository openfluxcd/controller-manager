/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/fluxcd/pkg/lockedfile"
	"github.com/fluxcd/pkg/sourceignore"
	pkgtar "github.com/fluxcd/pkg/tar"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/opencontainers/go-digest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/openfluxcd/artifact/api/v1alpha1"

	intdigest "github.com/openfluxcd/controller-manager/digest"
	sourcefs "github.com/openfluxcd/controller-manager/fs"
)

const GarbageCountLimit = 1000

const (
	// defaultFileMode is the permission mode applied to files inside an artifact archive.
	defaultFileMode int64 = 0o600
	// defaultDirMode is the permission mode applied to all directories inside an artifact archive.
	defaultDirMode int64 = 0o750
	// defaultExeFileMode is the permission mode applied to executable files inside an artifact archive.
	defaultExeFileMode int64 = 0o700
)

type Storer interface {
	// ReconcileStorage is responsible for setting up Storage data like URLs.
	ReconcileStorage(ctx context.Context, obj Collectable) error
	ReconcileArtifact(ctx context.Context, obj Collectable, revision, dir, hash string, archiveFunc func(*v1.Artifact, string) error) error
}

var _ Storer = &Storage{}

// Storage manages artifacts
type Storage struct {
	// BasePath is the local directory path where the source artifacts are stored.
	BasePath string `json:"basePath"`

	// Hostname is the file server host name used to compose the artifacts URIs.
	Hostname string `json:"hostname"`

	// ArtifactRetentionTTL is the duration of time that artifacts will be kept
	// in storage before being garbage collected.
	ArtifactRetentionTTL time.Duration `json:"artifactRetentionTTL"`

	// ArtifactRetentionRecords is the maximum number of artifacts to be kept in
	// storage after a garbage collection.
	ArtifactRetentionRecords int `json:"artifactRetentionRecords"`

	// kclient defines the Kubernetes kclient to find artifacts with.
	kclient client.Client
	// scheme contains the Kubernetes scheme to create objects for.
	scheme *runtime.Scheme
}

func (s *Storage) ReconcileArtifact(ctx context.Context, obj Collectable, revision, dir, filename string, archiveFunc func(*v1.Artifact, string) error) error {
	curArtifact, err := s.findArtifact(ctx, obj)
	if err != nil {
		return fmt.Errorf("failed to find artifact: %w", err)
	}

	// The artifact is up-to-date
	// Since digest is set by the end of reconciling an artifact,
	// we'll know if the artifact was created anew or if it already existed.
	if curArtifact != nil && s.ArtifactExist(curArtifact) && HasRevision(curArtifact, revision) {
		return nil
	}

	// We don't need to check this here...
	// Create potential new artifact with current available metadata
	artifact := s.NewArtifactFor(obj.GetKind(), obj.GetObjectMeta(), revision, filename)

	curArtifact = artifact.DeepCopy()

	// Ensure target path exists and is a directory
	if f, err := os.Stat(dir); err != nil {
		return fmt.Errorf("failed to stat target artifact path: %w", err)
	} else if !f.IsDir() {
		return fmt.Errorf("invalid target path: '%s' is not a directory", dir)
	}

	// Ensure artifact directory exists and acquire lock
	if err := s.MkdirAll(curArtifact); err != nil {
		return fmt.Errorf("failed to create artifact directory: %w", err)
	}

	unlock, err := s.Lock(curArtifact)
	if err != nil {
		return fmt.Errorf("failed to acquire lock for artifact: %w", err)
	}
	defer unlock()

	if err := archiveFunc(curArtifact, dir); err != nil {
		return fmt.Errorf("failed to archive artifact: %w", err)
	}

	createArtifact := curArtifact.DeepCopy()
	if _, err = controllerutil.CreateOrUpdate(ctx, s.kclient, createArtifact, func() error {
		if createArtifact.ObjectMeta.CreationTimestamp.IsZero() {
			if err := controllerutil.SetControllerReference(obj, createArtifact, s.scheme); err != nil {
				return fmt.Errorf("failed to set owner reference: %w", err)
			}
		}

		// mutate the existing object with the outside object.
		createArtifact.Spec.Revision = curArtifact.Spec.Revision
		createArtifact.Spec.Digest = curArtifact.Spec.Digest
		createArtifact.Spec.URL = curArtifact.Spec.URL
		createArtifact.Spec.LastUpdateTime = curArtifact.Spec.LastUpdateTime
		createArtifact.Spec.Size = curArtifact.Spec.Size

		return nil
	}); err != nil {
		return fmt.Errorf("failed to create/update artifact: %w", err)
	}

	return nil
}

// ReconcileStorage will do the following actions:
// - garbage collect old files
// - verify digest if the artifact does exist ( remove it if the digest doesn't match )
// - set the url of the artifact.
func (s *Storage) ReconcileStorage(ctx context.Context, obj Collectable) error {
	artifact, err := s.findArtifact(ctx, obj)
	if err != nil {
		return fmt.Errorf("failed to find artifact: %w", err)
	}

	// Garbage collect previous advertised artifact(s) from storage
	if err := s.garbageCollect(ctx, obj, artifact); err != nil {
		return fmt.Errorf("could not garbage collect artifact: %w", err)
	}

	if artifact == nil {
		return nil
	}

	// If the artifact is in storage, verify if the advertised digest still
	// matches the actual artifact
	if s.ArtifactExist(artifact) {
		if err := s.VerifyArtifact(artifact); err != nil {
			if err = s.Remove(artifact); err != nil {
				return fmt.Errorf("failed to remove artifact after digest mismatch: %w", err)
			}
		}
	}

	s.SetArtifactURL(artifact)

	return nil
}

// NewStorage creates the storage helper for a given path and hostname.
func NewStorage(client client.Client, scheme *runtime.Scheme, basePath string, hostname string, artifactRetentionTTL time.Duration, artifactRetentionRecords int) (*Storage, error) {
	if f, err := os.Stat(basePath); os.IsNotExist(err) || !f.IsDir() {
		return nil, fmt.Errorf("invalid dir path: %s", basePath)
	}

	return &Storage{
		BasePath:                 basePath,
		Hostname:                 hostname,
		ArtifactRetentionTTL:     artifactRetentionTTL,
		ArtifactRetentionRecords: artifactRetentionRecords,
		kclient:                  client,
		scheme:                   scheme,
	}, nil
}

// NewArtifactFor returns a new v1.Artifact.
func (s *Storage) NewArtifactFor(kind string, metadata metav1.Object, revision, fileName string) *v1.Artifact {
	urlBase := ArtifactURLBase(kind, metadata.GetNamespace(), metadata.GetName(), fileName)
	artifact := v1.Artifact{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.ToLower(kind + "-" + metadata.GetNamespace() + "-" + metadata.GetName()),
			Namespace: metadata.GetNamespace(),
		},
		Spec: v1.ArtifactSpec{
			URL:            urlBase,
			Revision:       revision,
			LastUpdateTime: metav1.Now(),
		},
	}

	s.SetArtifactURL(&artifact)
	return &artifact
}

// SetArtifactURL sets the URL on the given v1.Artifact.
// URL needs to include the location of the file.
func (s *Storage) SetArtifactURL(artifact *v1.Artifact) {
	if artifact.Spec.URL == "" {
		return
	}

	format := "http://%s/%s"
	if strings.HasPrefix(s.Hostname, "http://") || strings.HasPrefix(s.Hostname, "https://") {
		format = "%s/%s"
	}

	// New set the actual URL to the artifact using the URL base.
	basePath := s.LocalPathFromURL(artifact)
	artifact.Spec.URL = fmt.Sprintf(format, s.Hostname, strings.TrimLeft(basePath, "/"))
}

// SetHostname sets the hostname of the given URL string to the current Storage.Hostname and returns the result.
func (s *Storage) SetHostname(URL string) string {
	u, err := url.Parse(URL)
	if err != nil {
		return ""
	}
	u.Host = s.Hostname
	return u.String()
}

// MkdirAll calls os.MkdirAll for the given v1.Artifact base dir.
func (s *Storage) MkdirAll(artifact *v1.Artifact) error {
	dir := filepath.Dir(s.LocalPath(artifact))
	return os.MkdirAll(dir, 0o700)
}

// Remove calls os.Remove for the given v1.Artifact path.
func (s *Storage) Remove(artifact *v1.Artifact) error {
	return os.Remove(s.LocalPath(artifact))
}

// RemoveAll calls os.RemoveAll for the given v1.Artifact base dir.
func (s *Storage) RemoveAll(artifact *v1.Artifact) (string, error) {
	var deletedDir string
	dir := filepath.Dir(s.LocalPath(artifact))
	// Check if the dir exists.
	_, err := os.Stat(dir)
	if err == nil {
		deletedDir = dir
	}
	return deletedDir, os.RemoveAll(dir)
}

// RemoveAllButCurrent removes all files for the given v1.Artifact base dir, excluding the current one.
func (s *Storage) RemoveAllButCurrent(artifact *v1.Artifact) ([]string, error) {
	deletedFiles := []string{}
	localPath := s.LocalPath(artifact)
	dir := filepath.Dir(localPath)
	var errors []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			errors = append(errors, err.Error())
			return nil
		}

		if path != localPath && !info.IsDir() && info.Mode()&os.ModeSymlink != os.ModeSymlink {
			if err := os.Remove(path); err != nil {
				errors = append(errors, info.Name())
			} else {
				// Collect the successfully deleted file paths.
				deletedFiles = append(deletedFiles, path)
			}
		}
		return nil
	})

	if len(errors) > 0 {
		return deletedFiles, fmt.Errorf("failed to remove files: %s", strings.Join(errors, " "))
	}
	return deletedFiles, nil
}

// getGarbageFiles returns all files that need to be garbage collected for the given artifact.
// Garbage files are determined based on the below flow:
// 1. collect all artifact files with an expired ttl
// 2. if we satisfy maxItemsToBeRetained, then return
// 3. else, collect all artifact files till the latest n files remain, where n=maxItemsToBeRetained
func (s *Storage) getGarbageFiles(artifact *v1.Artifact, totalCountLimit, maxItemsToBeRetained int, ttl time.Duration) (garbageFiles []string, _ error) {
	localPath := s.LocalPath(artifact)
	if localPath == "" {
		return nil, nil
	}

	dir := filepath.Dir(localPath)
	if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
		return nil, nil
	}

	artifactFilesWithCreatedTs := make(map[time.Time]string)
	// sortedPaths contain all files sorted according to their created ts.
	var sortedPaths []string
	now := time.Now().UTC()
	totalArtifactFiles := 0
	var errors []string
	var creationTimestamps []time.Time
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			errors = append(errors, err.Error())
			return nil
		}
		if totalArtifactFiles >= totalCountLimit {
			return fmt.Errorf("reached file walking limit, already walked over: %d", totalArtifactFiles)
		}
		info, err := d.Info()
		if err != nil {
			errors = append(errors, err.Error())
			return nil
		}
		createdAt := info.ModTime().UTC()
		diff := now.Sub(createdAt)
		// Compare the time difference between now and the time at which the file was created
		// with the provided TTL. Delete if the difference is greater than the TTL. Since the
		// below logic just deals with determining if an artifact needs to be garbage collected,
		// we avoid all lock files, adding them at the end to the list of garbage files.
		expired := diff > ttl
		if !info.IsDir() && info.Mode()&os.ModeSymlink != os.ModeSymlink && filepath.Ext(path) != ".lock" {
			if path != localPath && expired {
				garbageFiles = append(garbageFiles, path)
			}
			totalArtifactFiles += 1
			artifactFilesWithCreatedTs[createdAt] = path
			creationTimestamps = append(creationTimestamps, createdAt)
		}
		return nil

	})
	if len(errors) > 0 {
		return nil, fmt.Errorf("can't walk over file: %s", strings.Join(errors, ","))
	}

	// We already collected enough garbage files to satisfy the no. of max
	// items that are supposed to be retained, so exit early.
	if totalArtifactFiles-len(garbageFiles) < maxItemsToBeRetained {
		return garbageFiles, nil
	}

	// sort all timestamps in ascending order.
	sort.Slice(creationTimestamps, func(i, j int) bool { return creationTimestamps[i].Before(creationTimestamps[j]) })
	for _, ts := range creationTimestamps {
		path, ok := artifactFilesWithCreatedTs[ts]
		if !ok {
			return garbageFiles, fmt.Errorf("failed to fetch file for created ts: %v", ts)
		}
		sortedPaths = append(sortedPaths, path)
	}

	var collected int
	noOfGarbageFiles := len(garbageFiles)
	for _, path := range sortedPaths {
		if path != localPath && filepath.Ext(path) != ".lock" && !stringInSlice(path, garbageFiles) {
			// If we previously collected some garbage files with an expired ttl, then take that into account
			// when checking whether we need to remove more files to satisfy the max no. of items allowed
			// in the filesystem, along with the no. of files already removed in this loop.
			if noOfGarbageFiles > 0 {
				if (len(sortedPaths) - collected - len(garbageFiles)) > maxItemsToBeRetained {
					garbageFiles = append(garbageFiles, path)
					collected += 1
				}
			} else {
				if len(sortedPaths)-collected > maxItemsToBeRetained {
					garbageFiles = append(garbageFiles, path)
					collected += 1
				}
			}
		}
	}

	return garbageFiles, nil
}

// GarbageCollect removes all garbage files in the artifact dir according to the provided
// retention options.
func (s *Storage) GarbageCollect(ctx context.Context, artifact *v1.Artifact, timeout time.Duration) ([]string, error) {
	delFilesChan := make(chan []string)
	errChan := make(chan error)
	// Abort if it takes more than the provided timeout duration.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	go func() {
		garbageFiles, err := s.getGarbageFiles(artifact, GarbageCountLimit, s.ArtifactRetentionRecords, s.ArtifactRetentionTTL)
		if err != nil {
			errChan <- err
			return
		}
		var errors []error
		var deleted []string
		if len(garbageFiles) > 0 {
			for _, file := range garbageFiles {
				err := os.Remove(file)
				if err != nil {
					errors = append(errors, err)
				} else {
					deleted = append(deleted, file)
				}
				// If a lock file exists for this garbage artifact, remove that too.
				lockFile := file + ".lock"
				if _, err = os.Lstat(lockFile); err == nil {
					err = os.Remove(lockFile)
					if err != nil {
						errors = append(errors, err)
					}
				}
			}
		}
		if len(errors) > 0 {
			errChan <- kerrors.NewAggregate(errors)
			return
		}
		delFilesChan <- deleted
	}()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case delFiles := <-delFilesChan:
			return delFiles, nil
		case err := <-errChan:
			return nil, err
		}
	}
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

// ArtifactExist returns a boolean indicating whether the v1.Artifact exists in storage and is a regular file.
func (s *Storage) ArtifactExist(artifact *v1.Artifact) bool {
	fi, err := os.Lstat(s.LocalPath(artifact))
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

// VerifyArtifact verifies if the Digest of the v1.Artifact matches the digest
// of the file in Storage. It returns an error if the digests don't match, or
// if it can't be verified.
func (s *Storage) VerifyArtifact(artifact *v1.Artifact) error {
	if artifact.Spec.Digest == "" {
		return fmt.Errorf("artifact has no digest")
	}

	d, err := digest.Parse(artifact.Spec.Digest)
	if err != nil {
		return fmt.Errorf("failed to parse artifact digest '%s': %w", artifact.Spec.Digest, err)
	}

	f, err := os.Open(s.LocalPath(artifact))
	if err != nil {
		return err
	}
	defer f.Close()

	verifier := d.Verifier()
	if _, err = io.Copy(verifier, f); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("computed digest doesn't match '%s'", d.String())
	}
	return nil
}

// ArchiveFileFilter must return true if a file should not be included in the archive after inspecting the given path
// and/or os.FileInfo.
type ArchiveFileFilter func(p string, fi os.FileInfo) bool

// SourceIgnoreFilter returns an ArchiveFileFilter that filters out files matching sourceignore.VCSPatterns and any of
// the provided patterns.
// If an empty gitignore.Pattern slice is given, the matcher is set to sourceignore.NewDefaultMatcher.
func SourceIgnoreFilter(ps []gitignore.Pattern, domain []string) ArchiveFileFilter {
	matcher := sourceignore.NewDefaultMatcher(ps, domain)
	if len(ps) > 0 {
		ps = append(sourceignore.VCSPatterns(domain), ps...)
		matcher = sourceignore.NewMatcher(ps)
	}
	return func(p string, fi os.FileInfo) bool {
		return matcher.Match(strings.Split(p, string(filepath.Separator)), fi.IsDir())
	}
}

// Archive atomically archives the given directory as a tarball to the given v1.Artifact path, excluding
// directories and any ArchiveFileFilter matches. While archiving, any environment specific data (for example,
// the user and group name) is stripped from file headers.
// If successful, it sets the digest and last update time on the artifact.
func (s *Storage) Archive(artifact *v1.Artifact, dir string, filter ArchiveFileFilter) (err error) {
	if f, err := os.Stat(dir); os.IsNotExist(err) || !f.IsDir() {
		return fmt.Errorf("invalid dir path: %s", dir)
	}

	localPath := s.LocalPath(artifact)
	tf, err := os.CreateTemp(filepath.Split(localPath))
	if err != nil {
		return err
	}
	tmpName := tf.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	d := intdigest.Canonical.Digester()
	sz := &writeCounter{}
	mw := io.MultiWriter(d.Hash(), tf, sz)

	gw := gzip.NewWriter(mw)
	tw := tar.NewWriter(gw)
	if err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Ignore anything that is not a file or directories e.g. symlinks
		if m := fi.Mode(); !(m.IsRegular() || m.IsDir()) {
			return nil
		}

		// Skip filtered files
		if filter != nil && filter(p, fi) {
			return nil
		}

		header, err := tar.FileInfoHeader(fi, p)
		if err != nil {
			return err
		}

		// The name needs to be modified to maintain directory structure
		// as tar.FileInfoHeader only has access to the base name of the file.
		// Ref: https://golang.org/src/archive/tar/common.go?#L626
		relFilePath := p
		if filepath.IsAbs(dir) {
			relFilePath, err = filepath.Rel(dir, p)
			if err != nil {
				return err
			}
		}
		sanitizeHeader(relFilePath, header)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}); err != nil {
		tw.Close()
		gw.Close()
		tf.Close()
		return err
	}

	if err := tw.Close(); err != nil {
		gw.Close()
		tf.Close()
		return err
	}
	if err := gw.Close(); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}

	if err := sourcefs.RenameWithFallback(tmpName, localPath); err != nil {
		return err
	}

	artifact.Spec.Digest = d.Digest().String()
	artifact.Spec.LastUpdateTime = metav1.Now()
	artifact.Spec.Size = &sz.written

	return nil
}

// AtomicWriteFile atomically writes the io.Reader contents to the v1.Artifact path.
// If successful, it sets the digest and last update time on the artifact.
func (s *Storage) AtomicWriteFile(artifact *v1.Artifact, reader io.Reader, mode os.FileMode) (err error) {
	localPath := s.LocalPath(artifact)
	tf, err := os.CreateTemp(filepath.Split(localPath))
	if err != nil {
		return err
	}
	tfName := tf.Name()
	defer func() {
		if err != nil {
			os.Remove(tfName)
		}
	}()

	d := intdigest.Canonical.Digester()
	sz := &writeCounter{}
	mw := io.MultiWriter(tf, d.Hash(), sz)

	if _, err := io.Copy(mw, reader); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tfName, mode); err != nil {
		return err
	}

	if err := sourcefs.RenameWithFallback(tfName, localPath); err != nil {
		return err
	}

	artifact.Spec.Digest = d.Digest().String()
	artifact.Spec.LastUpdateTime = metav1.Now()
	artifact.Spec.Size = &sz.written

	return nil
}

// Copy atomically copies the io.Reader contents to the v1.Artifact path.
// If successful, it sets the digest and last update time on the artifact.
func (s *Storage) Copy(artifact *v1.Artifact, reader io.Reader) (err error) {
	localPath := s.LocalPath(artifact)
	tf, err := os.CreateTemp(filepath.Split(localPath))
	if err != nil {
		return err
	}
	tfName := tf.Name()
	defer func() {
		if err != nil {
			os.Remove(tfName)
		}
	}()

	d := intdigest.Canonical.Digester()
	sz := &writeCounter{}
	mw := io.MultiWriter(tf, d.Hash(), sz)

	if _, err := io.Copy(mw, reader); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}

	if err := sourcefs.RenameWithFallback(tfName, localPath); err != nil {
		return err
	}

	artifact.Spec.Digest = d.Digest().String()
	artifact.Spec.LastUpdateTime = metav1.Now()
	artifact.Spec.Size = &sz.written

	return nil
}

// CopyFromPath atomically copies the contents of the given path to the path of the v1.Artifact.
// If successful, the digest and last update time on the artifact is set.
func (s *Storage) CopyFromPath(artifact *v1.Artifact, path string) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	err = s.Copy(artifact, f)
	return err
}

// CopyToPath copies the contents in the (sub)path of the given artifact to the given path.
func (s *Storage) CopyToPath(artifact *v1.Artifact, subPath, toPath string) error {
	// create a tmp directory to store artifact
	tmp, err := os.MkdirTemp("", "flux-include-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// read artifact file content
	localPath := s.LocalPath(artifact)
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// untar the artifact
	untarPath := filepath.Join(tmp, "unpack")
	if err = pkgtar.Untar(f, untarPath, pkgtar.WithMaxUntarSize(-1)); err != nil {
		return err
	}

	// create the destination parent dir
	if err = os.MkdirAll(filepath.Dir(toPath), os.ModePerm); err != nil {
		return err
	}

	// copy the artifact content to the destination dir
	fromPath, err := securejoin.SecureJoin(untarPath, subPath)
	if err != nil {
		return err
	}
	if err := sourcefs.RenameWithFallback(fromPath, toPath); err != nil {
		return err
	}
	return nil
}

// Symlink creates or updates a symbolic link for the given v1.Artifact and returns the URL for the symlink.
func (s *Storage) Symlink(artifact *v1.Artifact, linkName string) (string, error) {
	localPath := s.LocalPath(artifact)
	dir := filepath.Dir(localPath)
	link := filepath.Join(dir, linkName)
	tmpLink := link + ".tmp"

	if err := os.Remove(tmpLink); err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if err := os.Symlink(localPath, tmpLink); err != nil {
		return "", err
	}

	if err := os.Rename(tmpLink, link); err != nil {
		return "", err
	}

	return fmt.Sprintf("http://%s/%s", s.Hostname, filepath.Join(filepath.Dir(s.LocalPathFromURL(artifact)), linkName)), nil
}

// Lock creates a file lock for the given v1.Artifact.
func (s *Storage) Lock(artifact *v1.Artifact) (unlock func(), err error) {
	lockFile := s.LocalPath(artifact) + ".lock"
	mutex := lockedfile.MutexAt(lockFile)
	return mutex.Lock()
}

// LocalPath returns the secure local path of the given artifact (that is: relative to the Storage.BasePath).
func (s *Storage) LocalPath(artifact *v1.Artifact) string {
	if artifact.Spec.URL == "" {
		return ""
	}

	path, err := securejoin.SecureJoin(s.BasePath, s.LocalPathFromURL(artifact))
	if err != nil {
		return ""
	}

	return path
}

// LocalPathFromURL returns the local path on the file-system given the URL of the artifact.
func (s *Storage) LocalPathFromURL(artifact *v1.Artifact) string {
	// The URL without the hostname should end up using the right path on the filesystem.
	// We only trim at the beginning!
	actualFilePath := strings.TrimPrefix(artifact.Spec.URL, "http://")
	actualFilePath = strings.TrimPrefix(actualFilePath, "https://")
	actualFilePath = strings.TrimPrefix(actualFilePath, s.Hostname)

	return actualFilePath
}

// this should most likely be extracted into the controller-manager
func (s *Storage) findArtifact(ctx context.Context, object client.Object) (*v1.Artifact, error) {
	// this should look through ALL the artifacts and look if the owner is THIS object.
	list := &v1.ArtifactList{}
	if err := s.kclient.List(ctx, list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil, fmt.Errorf("failed to list artifacts: %w", err)
	}

	for _, artifact := range list.Items {
		if len(artifact.GetOwnerReferences()) != 1 {
			// ignore artifacts with multiple owners -> this should throw an error?
			continue
		}

		for _, owner := range artifact.OwnerReferences {
			if owner.Name == object.GetName() {
				return &artifact, nil
			}
		}
	}

	return nil, nil
}

// writeCounter is an implementation of io.Writer that only records the number
// of bytes written.
type writeCounter struct {
	written int64
}

func (wc *writeCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.written += int64(n)
	return n, nil
}

// sanitizeHeader modifies the tar.Header to be relative to the root of the
// archive and removes any environment specific data.
func sanitizeHeader(relP string, h *tar.Header) {
	// Modify the name to be relative to the root of the archive,
	// this ensures we maintain the same structure when extracting.
	h.Name = relP

	// We want to remove any environment specific data as well, this
	// ensures the checksum is purely content based.
	h.Gid = 0
	h.Uid = 0
	h.Uname = ""
	h.Gname = ""
	h.ModTime = time.Time{}
	h.AccessTime = time.Time{}
	h.ChangeTime = time.Time{}

	// Override the mode to be the default for the type of file.
	setDefaultMode(h)
}

// setDefaultMode sets the default mode for the given header.
func setDefaultMode(h *tar.Header) {
	if h.FileInfo().IsDir() {
		h.Mode = defaultDirMode
		return
	}

	if h.FileInfo().Mode().IsRegular() {
		mode := h.FileInfo().Mode()
		if mode&os.ModeType == 0 && mode&0o111 != 0 {
			h.Mode = defaultExeFileMode
			return
		}
		h.Mode = defaultFileMode
		return
	}
}

type Collectable interface {
	client.Object

	GetDeletionTimestamp() *metav1.Time
	GetObjectMeta() *metav1.ObjectMeta
	GetKind() string
}

// garbageCollect will delete old files.
func (s *Storage) garbageCollect(ctx context.Context, obj Collectable, artifact *v1.Artifact) error {
	if !obj.GetDeletionTimestamp().IsZero() {
		if _, err := s.RemoveAll(s.NewArtifactFor(obj.GetKind(), obj.GetObjectMeta(), "", "*")); err != nil {
			return fmt.Errorf("garbage collection for deleted resource failed: %w", err)
		}
		return nil
	}
	if artifact == nil {
		return nil
	}

	if _, err := s.GarbageCollect(ctx, artifact, time.Second*5); err != nil {
		return fmt.Errorf("garbage collection of artifacts failed: %w", err)
	}

	return nil
}

func HasRevision(artifact *v1.Artifact, revision string) bool {
	if artifact == nil {
		return false
	}

	return artifact.Spec.Revision == revision
}

// HasDigest returns if the given digest matches the current Digest of the
// Artifact.
func HasDigest(in *v1.Artifact, digest string) bool {
	if in == nil || in.Spec.Digest == "" {
		return false
	}

	return in.Spec.Digest == digest
}

// ArtifactURLBase returns the artifact url base path in the form of
// '<kind>/<namespace>/name>/<filename>'.
func ArtifactURLBase(kind, namespace, name, filename string) string {
	kind = strings.ToLower(kind)
	return path.Join(kind, namespace, name, filename)
}
