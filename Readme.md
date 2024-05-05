# NPM

[![Go Reference](https://pkg.go.dev/badge/github.com/livebud/npm.svg)](https://pkg.go.dev/github.com/livebud/npm)

Simple NPM client for installing `node_modules`.

## Features

- Makes direct HTTPS calls to the NPM registry
- You don't need NPM installed
- Less logging

## Install

```sh
go get github.com/livebud/npm
```

## Example

Install specific packages:

```go
npm.Install(ctx, dir, "react@18.2.0", "react-dom@18.2.0")
```

Install from a `package.json` in `dir`:

```json
{
  "dependencies": {
    "preact": "10.19.4",
    "preact-render-to-string": "6.3.1",
    "@lukeed/uuid": "^2.0.1"
  }
}
```

```go
npm.Install(ctx, dir)
```

## Contributors

- Matt Mueller ([@mattmueller](https://twitter.com/mattmueller))

## License

MIT
