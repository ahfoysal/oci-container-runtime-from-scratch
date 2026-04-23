// Package image implements a minimal Docker Hub registry v2 client and local
// image store for myrun. It uses only the Go standard library:
//
//  1. Fetch an anonymous bearer token from auth.docker.io for the target repo.
//  2. Fetch the image manifest (negotiating both Docker v2 and OCI media types).
//     If the server returns a manifest list / image index, pick the entry that
//     matches linux/<GOARCH> and fetch the per-platform manifest.
//  3. Download each layer blob (gzipped tar) by digest into the store.
//  4. Write manifest.json alongside the layer blobs.
//
// The store layout is:
//
//	data/images/<name>/<tag>/
//	    manifest.json
//	    layers/sha256/<hex>.tar.gz
//	    rootfs/<hex>/            (extracted, for overlay lowerdirs)
//
// Extraction is deliberately done at pull time so `myrun run` stays cheap.
package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Reference is a parsed image reference like "alpine:3.20" or
// "library/alpine:latest". Docker Hub expects the "library/" prefix for
// official images; we add it if missing.
type Reference struct {
	// Repo is the registry repository, e.g. "library/alpine".
	Repo string
	// Tag is the image tag, e.g. "3.20". Defaults to "latest".
	Tag string
}

// ParseRef splits "name[:tag]" into a Reference, injecting the library/
// namespace for single-segment names (the Docker Hub convention).
func ParseRef(s string) (Reference, error) {
	if s == "" {
		return Reference{}, errors.New("empty image reference")
	}
	name, tag := s, "latest"
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "/") {
		name, tag = s[:i], s[i+1:]
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return Reference{Repo: name, Tag: tag}, nil
}

// String renders the reference back into "<repo>:<tag>".
func (r Reference) String() string { return r.Repo + ":" + r.Tag }

// Media types we accept from the registry. We ask for all four so we can work
// with both classic Docker images and modern OCI images, single-arch or
// multi-arch (index/manifest-list).
const (
	mediaManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaManifestListV2 = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
)

// manifest is the common shape of a Docker v2 / OCI image manifest — enough
// fields for us to pull the config + layer blobs.
type manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// index is the common shape of a manifest list / OCI image index — an array
// of per-platform manifest descriptors.
type index struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Manifests     []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
		Platform  struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

// tokenResp is the subset of the auth.docker.io token endpoint we care about.
type tokenResp struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// Client is a Docker Hub registry v2 client. Zero-value is usable; it creates
// a default http.Client lazily.
type Client struct {
	HTTP *http.Client
}

// httpClient returns the configured client or http.DefaultClient.
func (c *Client) httpClient() *http.Client {
	if c == nil || c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

// token fetches an anonymous bearer token scoped to pull the given repo.
func (c *Client) token(repo string) (string, error) {
	u := "https://auth.docker.io/token?service=registry.docker.io&scope=" +
		url.QueryEscape("repository:"+repo+":pull")
	resp, err := c.httpClient().Get(u)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token: status %d: %s", resp.StatusCode, body)
	}
	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("token decode: %w", err)
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	return tr.AccessToken, nil
}

// authedGet issues a GET to the registry with the bearer token and the given
// Accept header(s) set.
func (c *Client) authedGet(u, token string, accepts ...string) (*http.Response, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for _, a := range accepts {
		req.Header.Add("Accept", a)
	}
	return c.httpClient().Do(req)
}

// Pull downloads the image identified by ref into storeRoot. Returns the
// on-disk path of the image (storeRoot/images/<repo>/<tag>/).
func (c *Client) Pull(ref Reference, storeRoot string) (string, error) {
	tok, err := c.token(ref.Repo)
	if err != nil {
		return "", err
	}

	manifestURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", ref.Repo, ref.Tag)
	resp, err := c.authedGet(manifestURL, tok,
		mediaManifestV2, mediaOCIManifest, mediaManifestListV2, mediaOCIIndex)
	if err != nil {
		return "", fmt.Errorf("manifest: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("manifest read: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("manifest: status %d: %s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	manifestBytes := body

	// If we got a list/index, pick the entry for linux/<GOARCH> and re-fetch.
	if strings.Contains(ct, "manifest.list") || strings.Contains(ct, "image.index") {
		var idx index
		if err := json.Unmarshal(body, &idx); err != nil {
			return "", fmt.Errorf("index decode: %w", err)
		}
		want := runtime.GOARCH
		var digest string
		for _, m := range idx.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == want {
				digest = m.Digest
				break
			}
		}
		if digest == "" {
			// Fall back to amd64 if the host arch isn't published.
			for _, m := range idx.Manifests {
				if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
					digest = m.Digest
					break
				}
			}
		}
		if digest == "" {
			return "", fmt.Errorf("no linux/%s manifest in index", want)
		}
		subURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", ref.Repo, digest)
		sub, err := c.authedGet(subURL, tok, mediaManifestV2, mediaOCIManifest)
		if err != nil {
			return "", fmt.Errorf("sub-manifest: %w", err)
		}
		manifestBytes, err = io.ReadAll(sub.Body)
		sub.Body.Close()
		if err != nil {
			return "", fmt.Errorf("sub-manifest read: %w", err)
		}
		if sub.StatusCode != 200 {
			return "", fmt.Errorf("sub-manifest: status %d: %s", sub.StatusCode, manifestBytes)
		}
	}

	var mf manifest
	if err := json.Unmarshal(manifestBytes, &mf); err != nil {
		return "", fmt.Errorf("manifest decode: %w", err)
	}
	if len(mf.Layers) == 0 {
		return "", errors.New("manifest has no layers")
	}

	store, err := OpenStore(storeRoot)
	if err != nil {
		return "", err
	}
	imgDir, err := store.ImageDir(ref)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(imgDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}

	for i, layer := range mf.Layers {
		fmt.Printf("  layer %d/%d %s (%d bytes)\n", i+1, len(mf.Layers), shortDigest(layer.Digest), layer.Size)
		blobPath, err := c.pullBlob(ref.Repo, layer.Digest, tok, store)
		if err != nil {
			return "", fmt.Errorf("pull layer %s: %w", layer.Digest, err)
		}
		if err := extractLayer(blobPath, store.RootfsDir(imgDir, layer.Digest)); err != nil {
			return "", fmt.Errorf("extract layer %s: %w", layer.Digest, err)
		}
	}

	return imgDir, nil
}

// pullBlob streams a layer blob to disk under the store's blob cache,
// verifying the sha256 digest. Returns the on-disk path.
func (c *Client) pullBlob(repo, digest, token string, store *Store) (string, error) {
	dst := store.BlobPath(digest)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // already cached
	}

	u := fmt.Sprintf("https://registry-1.docker.io/v2/%s/blobs/%s", repo, digest)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("blob: status %d: %s", resp.StatusCode, body)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		os.Remove(tmp)
		return "", fmt.Errorf("digest mismatch: want %s got %s", digest, got)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// extractLayer untars a gzipped layer blob into dst. Symlinks, hardlinks,
// directory modes, and AUFS/OCI whiteout files (.wh.*) are handled minimally:
// a `.wh.foo` entry removes `foo` from the target directory (mimicking how
// overlay's upperdir would mark a deletion), and `.wh..wh..opq` marks an
// opaque directory — we just skip it since we're extracting each layer into
// its own dir and overlay stacks them at mount time.
func extractLayer(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		// Sanitize to block tar path traversal.
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue
		}
		target := filepath.Join(dst, name)

		base := filepath.Base(name)
		if base == ".wh..wh..opq" {
			continue // opaque marker
		}
		if strings.HasPrefix(base, ".wh.") {
			// whiteout: remove <dir>/<orig> in this layer's tree
			orig := filepath.Join(filepath.Dir(target), strings.TrimPrefix(base, ".wh."))
			os.RemoveAll(orig)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o7777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o7777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			if err := os.Link(filepath.Join(dst, hdr.Linkname), target); err != nil {
				// Hardlinks across extractions can fail; ignore rather than abort.
				continue
			}
		default:
			// char/block/fifo/etc: ignore — busybox/alpine don't need them for
			// the MVP. A fuller runtime would honor them.
		}
	}
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
