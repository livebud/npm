package npm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/matthewmueller/glob"
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
	pkg, err := resolvePackage(pkgname)
	if err != nil {
		return err
	}
	if err := pkg.Install(ctx, dir); err != nil {
		return fmt.Errorf("npm install %s: %w", pkgname, err)
	}
	return nil
}

type Installable interface {
	Install(ctx context.Context, to string) error
}

func resolvePackage(pkgname string) (Installable, error) {
	if isLocal(pkgname) {
		return &localPackage{Path: pkgname}, nil
	}
	index := strings.LastIndex(pkgname, "@")
	if index == -1 {
		return nil, fmt.Errorf("npm: unable to install %[1]s because it's missing the version (e.g. %[1]s@1.0.0)", pkgname)
	}
	name, version := pkgname[:index], pkgname[index+1:]
	scope := ""
	index = strings.LastIndex(name, "/")
	if index != -1 {
		scope, name = name[:index], name[index+1:]
	}
	if version == "" {
		return nil, fmt.Errorf("npm: unable to install %[1]s because it's missing the version (e.g. %[1]s@1.0.0)", pkgname)
	} else if version == "latest" {
		return nil, fmt.Errorf("npm: unable to install %[1]s because tagged versions aren't supported yet", pkgname)
	}
	return &remotePackage{
		Scope:   scope,
		Name:    name,
		Version: version,
	}, nil
}

type remotePackage struct {
	Scope   string `json:"scope,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

var _ Installable = (*remotePackage)(nil)

func (p *remotePackage) url() string {
	if p.Scope == "" {
		return fmt.Sprintf(`https://registry.npmjs.org/%[1]s/-/%[1]s-%[2]s.tgz`, p.Name, p.Version)
	}
	return fmt.Sprintf(`https://registry.npmjs.org/%[1]s/%[2]s/-/%[2]s-%[3]s.tgz`, p.Scope, p.Name, p.Version)
}

func (p *remotePackage) dir(root string) string {
	if p.Scope == "" {
		return filepath.Join(root, "node_modules", p.Name)
	}
	return filepath.Join(root, "node_modules", p.Scope, p.Name)
}

func (p *remotePackage) Install(ctx context.Context, to string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.url(), nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("npm install %s: unexpected status code %d", p.Name, res.StatusCode)
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
		dir := filepath.Join(p.dir(to), rootless(filepath.Dir(header.Name)))
		filename := filepath.Join(dir, fileInfo.Name())
		if fileInfo.IsDir() {
			if err := os.MkdirAll(filename, fileInfo.Mode()); err != nil {
				return err
			}
			continue
		}
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileInfo.Mode())
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

type localPackage struct {
	Path string `json:"path,omitempty"`
}

// Ignored paths when installing local packages.
// Based on: https://github.com/npm/npm-packlist/blob/8c182a187e71c9f39b5558c2f6846c9161286441/lib/index.js#L14
var ignorePaths = map[string]bool{
	"node_modules":  true,
	".git":          true,
	".DS_Store":     true,
	".npmignore":    true,
	".gitignore":    true,
	".npmrc":        true,
	"npm-debug.log": true,
}

// Install local package to the given directory. This is a very limited
// implementation.
// TODO: better align with: https://github.com/npm/npm-packlist
func (p *localPackage) Install(ctx context.Context, to string) error {
	pkgPath := p.Path
	if filepath.IsLocal(pkgPath) {
		pkgPath = filepath.Join(to, p.Path)
	}
	manifestName := "package.json"
	manifestJson, err := os.ReadFile(filepath.Join(pkgPath, manifestName))
	if err != nil {
		return err
	}
	var manifest localManifest
	if err := json.Unmarshal(manifestJson, &manifest); err != nil {
		return err
	}
	fileMap := map[string]bool{
		manifestName: true,
	}
	if manifest.Main != "" {
		fileMap[filepath.Clean(manifest.Main)] = true
	}
	if manifest.Browser != "" {
		fileMap[filepath.Clean(manifest.Browser)] = true
	}
	for _, file := range manifest.Files {
		err := glob.Walk(filepath.Join(pkgPath, file+"**"), func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := de.Name()
			if hiddenPath(name) || ignorePaths[name] {
				if de.IsDir() {
					return fs.SkipDir
				}
				return nil
			} else if de.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(pkgPath, path)
			if err != nil {
				return err
			}
			fileMap[rel] = true
			return nil
		})
		if err != nil {
			return err
		}
	}
	files := make([]string, len(fileMap))
	i := 0
	for file := range fileMap {
		files[i] = file
		i++
	}
	nodeDir := filepath.Join(to, "node_modules", manifest.Name)
	return copyFiles(pkgPath, nodeDir, files...)
}

func copyFiles(from, to string, files ...string) error {
	eg := new(errgroup.Group)
	for _, file := range files {
		file := file
		eg.Go(func() error {
			return copyFile(filepath.Join(from, file), filepath.Join(to, file))
		})
	}
	return eg.Wait()
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()
	if _, err = io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	return nil
}

type localManifest struct {
	Name    string   `json:"name,omitempty"`
	Main    string   `json:"main,omitempty"`
	Browser string   `json:"browser,omitempty"`
	Files   []string `json:"files,omitempty"`
}

func rootless(fpath string) string {
	parts := strings.Split(fpath, string(filepath.Separator))
	return path.Join(parts[1:]...)
}

func isLocal(pkgname string) bool {
	return strings.HasPrefix(pkgname, ".") || strings.HasPrefix(pkgname, "/")
}

func hiddenPath(p string) bool {
	return strings.HasPrefix(p, ".")
}
