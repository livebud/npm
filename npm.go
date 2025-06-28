package npm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type Manifest struct {
	Name    string                       `json:"name,omitempty"`
	Main    string                       `json:"main,omitempty"`
	Browser string                       `json:"browser,omitempty"`
	Files   []string                     `json:"files,omitempty"`
	Imports map[string]map[string]string `json:"imports,omitempty"`
	// TODO: this can also be a string or list
	Exports      map[string]string `json:"exports,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

func Install(ctx context.Context, dir string, packages ...string) error {
	eg := new(errgroup.Group)
	sg := new(singleflight.Group)

	if len(packages) == 0 {
		manifestPath := filepath.Join(dir, "package.json")
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
		for dep, version := range pkg.Dependencies {
			if isLocal(version) || isAbsolute(version) {
				packages = append(packages, version)
				continue
			}
			pkgname := fmt.Sprintf("%s@%s", dep, version)
			packages = append(packages, pkgname)
		}
	}

	for _, pkg := range packages {
		pkg := pkg
		eg.Go(func() error {
			return install(ctx, sg, dir, pkg)
		})
	}
	return eg.Wait()
}

func install(ctx context.Context, sg *singleflight.Group, dir string, pkgname string) error {
	pkg, err := resolvePackage(dir, pkgname)
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

func parseScope(pkgname string) (scope string, name string) {
	index := strings.LastIndex(pkgname, "/")
	if index == -1 {
		return "", pkgname
	}
	return pkgname[:index], pkgname[index+1:]
}

// Version resolves the version of a package. To get the latest you can do
// `version, err := npm.Version(ctx, "preact", "*")`.
func Version(ctx context.Context, pkgname, constraint string) (string, error) {
	version, err := resolveVersion(pkgname, constraint)
	if err != nil {
		return "", err
	}
	return version, nil
}

func resolvePackage(dir, pkgname string) (installable, error) {
	if isLocal(pkgname) {
		return readLocalPackage(filepath.Join(dir, pkgname))
	} else if isAbsolute(pkgname) {
		return readLocalPackage(pkgname)
	}
	index := strings.LastIndex(pkgname, "@")
	if index == -1 {
		return nil, fmt.Errorf("npm: unable to install %[1]s because it's missing the version (e.g. %[1]s@1.0.0)", pkgname)
	}
	pkgName, version := pkgname[:index], pkgname[index+1:]
	scope, name := parseScope(pkgName)
	if version == "" {
		return nil, fmt.Errorf("npm: unable to install %[1]s because it's missing the version (e.g. %[1]s@1.0.0)", pkgname)
	} else if version == "latest" {
		return nil, fmt.Errorf("npm: unable to install %[1]s because tagged versions aren't supported yet", pkgname)
	}
	version, err := resolveVersion(pkgName, version)
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

func resolveVersions(pkgName string) (semver.Collection, error) {
	req, err := http.NewRequest(http.MethodGet, `https://registry.npmjs.org/`+pkgName, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to create request to resolve version for %s: %w", pkgName, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unable to preform request to resolve version for %s: %w", pkgName, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code while resolving version for %s: %d", pkgName, res.StatusCode)
	}
	var pkg struct {
		Versions map[string]struct{} `json:"versions,omitempty"`
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read body while resolving version for %s: %w", pkgName, err)
	}
	if err := json.Unmarshal(body, &pkg); err != nil {
		return nil, fmt.Errorf("unable to unmarshal body while resolving version for %s: %w", pkgName, err)
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
	return versions, nil
}

func resolveVersion(pkgName, constraint string) (string, error) {
	versions, err := resolveVersions(pkgName)
	if err != nil {
		return "", fmt.Errorf("unable to resolve versions for %s: %w", pkgName, err)
	}
	checker, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("unable to create a new constraint for %s@%s: %w", pkgName, constraint, err)
	}
	for i := len(versions) - 1; i >= 0; i-- {
		if checker.Check(versions[i]) {
			return versions[i].String(), nil
		}
	}
	return "", fmt.Errorf("unable to resolve version for %s@%s: no matching version found", pkgName, constraint)
}

func readLocalPackage(pkgdir string) (*localPackage, error) {
	manifestPath := filepath.Join(pkgdir, "package.json")
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
		Path: pkgdir,
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
	var manifest Manifest
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
	for err, path := range glob.Walk(pkgPath, manifest.Files...) {
		if err != nil {
			return fmt.Errorf("error while walking %s to install local package: %w", path, err)
		}
		name := filepath.Base(path)
		if hiddenPath(name) || ignorePaths[name] {
			continue
		}
		rel, err := filepath.Rel(pkgPath, path)
		if err != nil {
			return fmt.Errorf("unable to get relative path for %s to install local package: %w", path, err)
		}
		fileMap[rel] = true
	}
	for _, imp := range manifest.Imports {
		for _, p := range imp {
			fileMap[filepath.Clean(p)] = true
		}
	}
	for _, p := range manifest.Exports {
		fileMap[filepath.Clean(p)] = true
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
	stat, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("unable to stat %s to copy: %w", src, err)
	}
	if stat.IsDir() {
		return copyDir(src, dst)
	}
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

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if hiddenPath(rel) || ignorePaths[filepath.Base(rel)] {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

func rootless(fpath string) string {
	parts := strings.Split(fpath, string(filepath.Separator))
	return path.Join(parts[1:]...)
}

func isLocal(pkgname string) bool {
	return strings.HasPrefix(pkgname, ".")
}

func isAbsolute(pkgname string) bool {
	return strings.HasPrefix(pkgname, "/")
}

func hiddenPath(p string) bool {
	return strings.HasPrefix(p, ".")
}
