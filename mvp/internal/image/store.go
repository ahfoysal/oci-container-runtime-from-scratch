package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is a local filesystem-backed image store. It owns a directory tree:
//
//	<root>/
//	    blobs/sha256/<hex>             raw layer tarballs (dedup across images)
//	    images/<repo>/<tag>/
//	        manifest.json
//	        rootfs/<hex>/              extracted layer contents
//
// We extract each layer into its own directory so OverlayFS can stack them as
// lowerdirs without copying. Blobs are content-addressed, so two images
// sharing a base layer share the tarball on disk.
type Store struct {
	Root string
}

// OpenStore ensures the root directory exists and returns a Store handle.
func OpenStore(root string) (*Store, error) {
	if root == "" {
		root = filepath.Join("data")
	}
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

// ImageDir returns the per-image directory, creating it if needed.
func (s *Store) ImageDir(ref Reference) (string, error) {
	d := filepath.Join(s.Root, "images", ref.Repo, ref.Tag)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// BlobPath returns the on-disk path for a layer blob by its digest.
func (s *Store) BlobPath(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(s.Root, "blobs", "sha256", hex)
}

// RootfsDir returns the extraction directory for a single layer under an
// image. Layers from the same image live as siblings so overlay can mount
// them together.
func (s *Store) RootfsDir(imgDir, digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(imgDir, "rootfs", hex)
}

// LoadManifest reads an image's manifest.json. Returns the decoded manifest
// plus the ordered list of layer directories (lowest first) suitable for
// passing to an overlay mount as `lowerdir=a:b:c` — note overlay expects
// upper/newer layers FIRST, so callers typically reverse this slice.
func (s *Store) LoadManifest(ref Reference) (*ImageInfo, error) {
	imgDir := filepath.Join(s.Root, "images", ref.Repo, ref.Tag)
	data, err := os.ReadFile(filepath.Join(imgDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	info := &ImageInfo{
		Ref:    ref,
		Dir:    imgDir,
		Config: mf.Config.Digest,
	}
	for _, l := range mf.Layers {
		info.Layers = append(info.Layers, s.RootfsDir(imgDir, l.Digest))
	}
	return info, nil
}

// ImageInfo is the post-pull view of an image on disk.
type ImageInfo struct {
	Ref    Reference
	Dir    string   // <root>/images/<repo>/<tag>/
	Config string   // config blob digest (not yet fetched; reserved for future)
	Layers []string // extracted layer dirs, lowest-first (base layer at index 0)
}

// OverlayLowerDirs returns layer dirs in the order OverlayFS expects:
// topmost (newest) first, base last. Joined with ':'.
func (i *ImageInfo) OverlayLowerDirs() string {
	// Reverse Layers.
	n := len(i.Layers)
	r := make([]string, n)
	for k, l := range i.Layers {
		r[n-1-k] = l
	}
	return strings.Join(r, ":")
}
