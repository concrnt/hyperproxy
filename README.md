# hyperproxy

build (requires libvips-dev; ffmpeg is needed at runtime for APNG):
```sh
go build
```

env:
- `HYPERPROXY_RESIZE_CONCURRENCY`: number of images resized at the same time (default: GOMAXPROCS)
