# hyperproxy

build:
```sh
CGO_CPPFLAGS="$(pkg-config --cflags Magick++)" CGO_LDFLAGS="$(pkg-config --libs Magick++)" go build
```

