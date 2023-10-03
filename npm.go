package npm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"
)

func Install(ctx context.Context, dir string, packages ...string) error {
	eg := new(errgroup.Group)
	for _, pkg := range packages {
		pkg := pkg
		eg.Go(func() error {
			return install(ctx, dir, pkg)
		})
	}
	return eg.Wait()
}

func install(ctx context.Context, dir string, pkgname string) error {
	pkg, err := resolvePkg(pkgname)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", pkg.URL(), nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("npm install %s: unexpected status code %d", pkg.Name, res.StatusCode)
	}
	gzipReader, err := gzip.NewReader(res.Body)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		fileInfo := header.FileInfo()
		dir := filepath.Join(pkg.Dir(dir), rootless(filepath.Dir(header.Name)))
		filename := filepath.Join(dir, fileInfo.Name())
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		file, err := os.Create(filename)
		if err != nil {
			return err
		}
		if _, err := io.Copy(file, tarReader); err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func resolvePkg(pkgname string) (resolved *pkg, err error) {
	parts := strings.Split(pkgname, "@")
	name := parts[0]
	version := ""
	if len(parts) >= 2 {
		version = parts[1]
	}
	if version == "" {
		return nil, fmt.Errorf("npm: unable to install %[1]s because it's missing the version (e.g. %[1]s@1.0.0)", pkgname)
	} else if version == "latest" {
		return nil, fmt.Errorf("npm: unable to install %[1]s because tagged versions aren't supported yet", pkgname)
	}
	return newPkg(name, version), nil
}

func newPkg(name, version string) *pkg {
	return &pkg{
		Name:         name,
		Version:      version,
		Dependencies: map[string]string{},
	}
}

type pkg struct {
	Name            string            `json:"name,omitempty"`
	Version         string            `json:"version,omitempty"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func (p *pkg) URL() string {
	return fmt.Sprintf(`https://registry.npmjs.org/%[1]s/-/%[1]s-%[2]s.tgz`, p.Name, p.Version)
}

func (p *pkg) Dir(root string) string {
	return filepath.Join(root, "node_modules", p.Name)
}

func rootless(fpath string) string {
	parts := strings.Split(fpath, string(filepath.Separator))
	return path.Join(parts[1:]...)
}
