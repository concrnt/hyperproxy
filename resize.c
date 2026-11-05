#include <stdio.h>
#include <string.h>
#include <vips/vips.h>

#include "resize.h"

int init_resize(void) {
    if (VIPS_INIT("hyperproxy")) {
        fprintf(stderr, "vips init failed: %s\n", vips_error_buffer());
        return 1;
    }
    // Cached files are evicted and rewritten under the same name, so never reuse results by filename.
    vips_cache_set_max(0);
    // Parallelism comes from concurrent requests (see resize.go), not from threads inside one image.
    vips_concurrency_set(1);
    // Blocks magickload and other loaders not meant for untrusted input.
    // SVG is flagged too, but it is the same librsvg the ImageMagick setup used, and `,webp` needs it.
    vips_block_untrusted_set(TRUE);
    vips_operation_block_set("VipsForeignLoadSvg", FALSE);
    return 0;
}

int resize(const char *input, const char *output, int width, int height) {
    const char *loader = vips_foreign_find_load(input);
    if (!loader) {
        fprintf(stderr, "    Error: %s\n", vips_error_buffer());
        vips_error_clear();
        return 1;
    }

    // Only these loaders take n=-1; load every frame so animations survive.
    char *filename = (strstr(loader, "gif") || strstr(loader, "Webp"))
        ? g_strdup_printf("%s[n=-1]", input)
        : g_strdup(input);

    VipsImage *out = NULL;
    int ret = vips_thumbnail(filename, &out, width > 0 ? width : VIPS_MAX_COORD,
        "height", height > 0 ? height : VIPS_MAX_COORD,
        "size", VIPS_SIZE_DOWN,
        NULL);
    g_free(filename);
    if (ret == 0) {
        ret = vips_webpsave(out, output, "Q", 70, NULL);
    }
    if (out) {
        g_object_unref(out);
    }
    if (ret != 0) {
        fprintf(stderr, "    Error: %s\n", vips_error_buffer());
        vips_error_clear();
        return 1;
    }
    return 0;
}
