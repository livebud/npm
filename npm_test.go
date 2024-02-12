package npm_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/livebud/npm"
	"github.com/matryer/is"
)

func exists(t testing.TB, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %q to exist: %s", path, err)
	}
}

func notExists(t testing.TB, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %q to not exist", path)
	}
}

func TestInstallSvelte(t *testing.T) {
	is := is.New(t)
	dir := t.TempDir()
	ctx := context.Background()
	err := npm.Install(ctx, dir, "svelte@3.42.3", "uid@2.0.0")
	is.NoErr(err)
	exists(t, filepath.Join(dir, "node_modules", "svelte", "package.json"))
	exists(t, filepath.Join(dir, "node_modules", "uid", "package.json"))
	exists(t, filepath.Join(dir, "node_modules", "svelte", "internal", "index.js"))
}

func TestInstallReact(t *testing.T) {
	is := is.New(t)
	dir := t.TempDir()
	ctx := context.Background()
	err := npm.Install(ctx, dir, "react@18.2.0", "react-dom@18.2.0")
	is.NoErr(err)
	exists(t, filepath.Join(dir, "node_modules", "react", "package.json"))
	exists(t, filepath.Join(dir, "node_modules", "react-dom", "package.json"))
}

func TestInstallStripe(t *testing.T) {
	is := is.New(t)
	dir := t.TempDir()
	ctx := context.Background()
	err := npm.Install(ctx, dir, "@stripe/stripe-js@2.1.11")
	is.NoErr(err)
	exists(t, filepath.Join(dir, "node_modules", "@stripe", "stripe-js", "package.json"))
}

func writeFiles(dir string, files map[string]string) error {
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

func equals(t testing.TB, path string, expected string) {
	t.Helper()
	code, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %q to exist: %s", path, err)
	}
	if string(code) != expected {
		t.Fatalf("expected %q to equal %q", path, expected)
	}
}

func TestInstallLocal(t *testing.T) {
	is := is.New(t)
	dir := t.TempDir()
	pkgDir := t.TempDir()
	files := map[string]string{
		"package.json": `{
			"name": "bud",
			"main": "./main.ts",
			"browser": "./browser.ts",
			"files": [
				"src/"
			]
		}`,
		"browser.ts":                    `export const browser = "browser"`,
		"main.ts":                       `export const main = "main"`,
		"src/index.ts":                  `export const bud = "bud"`,
		"src/another.ts":                `export const another = "another"`,
		"src/cool.js":                   `export const cool = "cool"`,
		"src/.hidden.js":                `export const hidden = "hidden"`,
		"src/.DS_Store":                 `{}`,
		"src/.git/hooks/precommit":      `{}`,
		"node_modules/uid/package.json": `{}`,
		".gitignore":                    `node_modules/`,
	}
	is.NoErr(writeFiles(pkgDir, files))
	ctx := context.Background()
	err := npm.Install(ctx, dir, pkgDir)
	is.NoErr(err)
	equals(t, filepath.Join(dir, "node_modules", "bud", "browser.ts"), files["browser.ts"])
	equals(t, filepath.Join(dir, "node_modules", "bud", "main.ts"), files["main.ts"])
	equals(t, filepath.Join(dir, "node_modules", "bud", "src", "index.ts"), files["src/index.ts"])
	equals(t, filepath.Join(dir, "node_modules", "bud", "src", "another.ts"), files["src/another.ts"])
	equals(t, filepath.Join(dir, "node_modules", "bud", "src", "cool.js"), files["src/cool.js"])
	notExists(t, filepath.Join(dir, "node_modules", "bud", "src", ".hidden.js"))
	notExists(t, filepath.Join(dir, "node_modules", "bud", "src", ".DS_Store"))
	notExists(t, filepath.Join(dir, "node_modules", "bud", "src", ".git"))
	notExists(t, filepath.Join(dir, "node_modules", "bud", "node_modules"))
	notExists(t, filepath.Join(dir, "node_modules", "bud", ".gitignore"))
}
