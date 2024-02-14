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
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/matthewmueller/glob"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

func Install(ctx context.Context, dir string, packages ...string) error {
	eg := new(errgroup.Group)
	sg := new(singleflight.Group)
	for _, pkg := range packages {
		pkg := pkg
		eg.Go(func() error {
			return install(ctx, sg, dir, pkg)
		})
	}
	return eg.Wait()
}

func install(ctx context.Context, sg *singleflight.Group, dir string, pkgname string) error {
	pkg, err := resolvePackage(pkgname)
	if err != nil {
		return err
	}
	// Only install a package once
	// TODO: this may need to get smarter to handle different versions
	_, err, _ = sg.Do(pkg.Key(), func() (interface{}, error) {
		if err := pkg.Install(ctx, sg, dir); err != nil {
			return nil, fmt.Errorf("npm install %s: %w", pkgname, err)
		}
		return nil, nil
	})
	return err
}

type installable interface {
	Key() string
	Install(ctx context.Context, sg *singleflight.Group, to string) error
}

func resolvePackage(pkgname string) (installable, error) {
	if isLocal(pkgname) {
		return readLocalPackage(pkgname)
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
	version, err := resolveVersion(scope, name, version)
	if err != nil {
		return nil, err
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

var _ installable = (*remotePackage)(nil)

func (p *remotePackage) Key() string {
	if p.Scope == "" {
		return p.Name
	}
	return fmt.Sprintf("%s/%s", p.Scope, p.Name)
}

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

func (p *remotePackage) Install(ctx context.Context, sg *singleflight.Group, to string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url(), nil)
	if err != nil {
		return fmt.Errorf("unable to create request for %s: %w", p.Name, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unable to download %s: %w", p.Name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("unexpected status code while installing %s: %d", p.Name, res.StatusCode)
	}
	gzipReader, err := gzip.NewReader(res.Body)
	if err != nil {
		return fmt.Errorf("unable to create gzip reader: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("unable to get next header: %w", err)
		}
		fileInfo := header.FileInfo()
		dir := filepath.Join(p.dir(to), rootless(filepath.Dir(header.Name)))
		filename := filepath.Join(dir, fileInfo.Name())
		if fileInfo.IsDir() {
			if err := os.MkdirAll(filename, fileInfo.Mode()); err != nil {
				return fmt.Errorf("unable to make directory %q from tarball: %w", filename, err)
			}
			continue
		}
		if err = os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("unable to make directory for file %q from tarball: %w", filename, err)
		}
		file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fileInfo.Mode())
		if err != nil {
			return fmt.Errorf("unable to open file %q from tarball: %w", filename, err)
		}
		if written, err := io.Copy(file, tarReader); err != nil {
			return fmt.Errorf("unable to copy file %q from tarball: %w", filename, err)
		} else if written != header.Size {
			return fmt.Errorf("unable to copy file %q from tarball: wrote %d bytes, expected %d", filename, written, header.Size)
		}
		if err = file.Close(); err != nil {
			return fmt.Errorf("unable to close file %q from tarball: %w", filename, err)
		}
	}
	// Install dependencies
	manifestPath := filepath.Join(p.dir(to), "package.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("unable to read package.json: %w", err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies,omitempty"`
	}
	if err := json.Unmarshal(manifest, &pkg); err != nil {
		return fmt.Errorf("unable to unmarshal package.json: %w", err)
	}
	eg := new(errgroup.Group)
	for dep, version := range pkg.Dependencies {
		pkgname := fmt.Sprintf("%s@%s", dep, version)
		eg.Go(func() error {
			return install(ctx, sg, to, pkgname)
		})
	}
	return eg.Wait()
}

func resolveVersion(scope, name, version string) (string, error) {
	if version == "latest" {
		return "latest", nil
	}
	pkgName := name
	if scope != "" {
		pkgName = fmt.Sprintf("%s/%s", scope, name)
	}
	req, err := http.NewRequest(http.MethodGet, `https://registry.npmjs.org/`+pkgName, nil)
	if err != nil {
		return "", fmt.Errorf("unable to create request to resolve version for %s@%s: %w", name, version, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("unable to preform request to resolve version for %s@%s: %w", name, version, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("unexpected status code while resolving version for %s@%s: %d", name, version, res.StatusCode)
	}
	var pkg struct {
		Versions map[string]struct{} `json:"versions,omitempty"`
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("unable to read body while resolving version for %s@%s: %w", name, version, err)
	}
	if err := json.Unmarshal(body, &pkg); err != nil {
		return "", fmt.Errorf("unable to unmarshal body while resolving version for %s@%s: %w", name, version, err)
	}
	var versions semver.Collection
	for version := range pkg.Versions {
		v, err := semver.NewVersion(version)
		if err != nil {
			// Ignore errors that might be in the NPM registry.
			continue
		}
		versions = append(versions, v)
	}
	sort.Sort(versions)
	constraint, err := semver.NewConstraint(version)
	if err != nil {
		return "", fmt.Errorf("unable to create a new constraint for %s@%s: %w", name, version, err)
	}
	for i := len(versions) - 1; i >= 0; i-- {
		if constraint.Check(versions[i]) {
			return versions[i].String(), nil
		}
	}
	return "", fmt.Errorf("unable to resolve version for %s@%s: no matching version found", name, version)
}

func readLocalPackage(dir string) (*localPackage, error) {
	manifestPath := filepath.Join(dir, "package.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read package.json: %w", err)
	}
	var pkg struct {
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(manifest, &pkg); err != nil {
		return nil, fmt.Errorf("unable to unmarshal package.json: %w", err)
	}
	return &localPackage{
		Name: pkg.Name,
		Path: dir,
	}, nil
}

type localPackage struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

var _ installable = (*localPackage)(nil)

func (p *localPackage) Key() string {
	return p.Name
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
func (p *localPackage) Install(ctx context.Context, sg *singleflight.Group, to string) error {
	pkgPath := p.Path
	if filepath.IsLocal(pkgPath) {
		pkgPath = filepath.Join(to, p.Path)
	}
	manifestName := "package.json"
	manifestJson, err := os.ReadFile(filepath.Join(pkgPath, manifestName))
	if err != nil {
		return fmt.Errorf("unable to read %s for %s: %w", manifestName, p.Path, err)
	}
	var manifest localManifest
	if err := json.Unmarshal(manifestJson, &manifest); err != nil {
		return fmt.Errorf("unable to unmarshal %s for %s: %w", manifestName, p.Path, err)
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
				return fmt.Errorf("error while walking %s to install local package: %w", path, err)
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
				return fmt.Errorf("unable to get relative path for %s to install local package: %w", path, err)
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
	if err := copyFiles(pkgPath, nodeDir, files...); err != nil {
		return fmt.Errorf("unable to copy files to install local package: %w", err)
	}
	eg := new(errgroup.Group)
	for dep, version := range manifest.Dependencies {
		pkgname := fmt.Sprintf("%s@%s", dep, version)
		eg.Go(func() error {
			return install(ctx, sg, to, pkgname)
		})
	}
	return eg.Wait()
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
		return fmt.Errorf("unable to open %s to copy: %w", src, err)
	}
	defer srcFile.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("unable to make directory for %s to copy: %w", dst, err)
	}
	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("unable to create %s to copy: %w", dst, err)
	}
	defer dstFile.Close()
	if _, err = io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("unable to copy %s to %s: %w", src, dst, err)
	}
	return nil
}

type localManifest struct {
	Name         string            `json:"name,omitempty"`
	Main         string            `json:"main,omitempty"`
	Browser      string            `json:"browser,omitempty"`
	Files        []string          `json:"files,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
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
