/* SPDX-License-Identifier: Apache-2.0 OR MIT */
#include "shim.h"

#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

/* Defined in Go (callback.go). */
extern int32_t goLataiEvent(uintptr_t handle, char *json, size_t len);

static const LataiApi *api;

void lg_free(void *p) { free(p); }

static void set(lg_str *s, const char *text, size_t len) {
    s->ptr = malloc(len + 1);
    if (s->ptr == NULL) {
        s->len = 0;
        return;
    }
    memcpy(s->ptr, text, len);
    s->ptr[len] = '\0';
    s->len = len;
}

static void set_text(lg_str *s, const char *text) { set(s, text, strlen(text)); }

int lg_load(const char *path, uint32_t version, lg_str *err) {
    if (api != NULL) {
        return 0;
    }
    typedef const LataiApi *(*get_api_fn)(uint32_t);
    get_api_fn get = NULL;
#ifdef _WIN32
    int wide = MultiByteToWideChar(CP_UTF8, 0, path, -1, NULL, 0);
    wchar_t *wpath = malloc(sizeof(wchar_t) * (size_t)(wide > 0 ? wide : 1));
    if (wpath == NULL) {
        set_text(err, "out of memory");
        return 1;
    }
    MultiByteToWideChar(CP_UTF8, 0, path, -1, wpath, wide);
    HMODULE lib = LoadLibraryExW(wpath, NULL, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_DEFAULT_DIRS);
    free(wpath);
    if (lib == NULL) {
        set_text(err, "LoadLibrary failed");
        return 1;
    }
    get = (get_api_fn)(void (*)(void))GetProcAddress(lib, "latai_get_api");
#else
    void *lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (lib == NULL) {
        const char *e = dlerror();
        set_text(err, e != NULL ? e : "dlopen failed");
        return 1;
    }
    *(void **)(&get) = dlsym(lib, "latai_get_api");
#endif
    if (get == NULL) {
        set_text(err, "the library has no latai_get_api");
        return 1;
    }
    const LataiApi *table = get(version);
    if (table == NULL) {
        set_text(err, "the library does not serve this API version");
        return 1;
    }
    api = table;
    return 0;
}

uint32_t lg_version(void) { return api != NULL ? api->version : 0; }

/* The thread's last error, read right after the call that failed. */
static int32_t fail(int32_t rc, lg_str *err) {
    if (rc != LATAI_OK && err != NULL) {
        size_t len = 0;
        if (api->last_error(NULL, &len) == LATAI_OK && len > 0) {
            char *buf = malloc(len);
            if (buf != NULL) {
                size_t cap = len;
                if (api->last_error(buf, &cap) == LATAI_OK && cap <= len) {
                    set(err, buf, strlen(buf));
                }
                free(buf);
            }
        }
    }
    return rc;
}

/* Copy a library string into `out` and release it with the table's own free. */
static void take(char *ptr, size_t len, lg_str *out) {
    if (ptr != NULL) {
        set(out, ptr, len);
        api->free(ptr);
    }
}

int32_t lg_open(const char *cfg, size_t len, LataiRuntime **rt, lg_str *err) {
    return fail(api->open(cfg, len, rt), err);
}

int32_t lg_close(LataiRuntime *rt, lg_str *err) { return fail(api->close(rt), err); }

int32_t lg_info(LataiRuntime *rt, lg_str *out, lg_str *err) {
    for (int attempt = 0; attempt < 4; attempt++) {
        size_t len = 0;
        int32_t rc = api->info(rt, NULL, &len);
        if (rc != LATAI_OK) {
            return fail(rc, err);
        }
        char *buf = malloc(len);
        if (buf == NULL) {
            return 1;
        }
        size_t cap = len;
        rc = api->info(rt, buf, &cap);
        if (rc != LATAI_OK) {
            free(buf);
            return fail(rc, err);
        }
        if (cap <= len) {
            set(out, buf, strlen(buf));
            free(buf);
            return LATAI_OK;
        }
        free(buf); /* grew between the two calls: ask again */
    }
    return 1;
}

int32_t lg_run(LataiRuntime *rt, const char *req, size_t len, const char *id, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc = fail(api->run(rt, req, len, id, &p, &n), err);
    take(p, n, out);
    return rc;
}

static int32_t trampoline(void *user_data, const char *json, size_t len) {
    return goLataiEvent((uintptr_t)user_data, (char *)json, len);
}

int32_t lg_run_stream(LataiRuntime *rt, const char *req, size_t len, const char *id, uintptr_t handle, lg_str *err) {
    return fail(api->run_stream(rt, req, len, id, trampoline, (void *)handle), err);
}

int32_t lg_cancel(LataiRuntime *rt, const char *id, lg_str *err) { return fail(api->cancel(rt, id), err); }

int32_t lg_session_close(LataiRuntime *rt, const char *session, lg_str *err) {
    return fail(api->session_close(rt, session), err);
}

int32_t lg_call1(int which, LataiRuntime *rt, const char *arg, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc;
    switch (which) {
    case 0: rc = api->inspect(rt, arg, &p, &n); break;
    case 1: rc = api->stage(rt, arg, &p, &n); break;
    case 2: rc = api->rollback(rt, arg, &p, &n); break;
    default: rc = api->status(rt, arg, &p, &n); break;
    }
    rc = fail(rc, err);
    take(p, n, out);
    return rc;
}

int32_t lg_pull(LataiRuntime *rt, const char *url, int32_t force, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc = fail(api->pull(rt, url, force, &p, &n), err);
    take(p, n, out);
    return rc;
}

int32_t lg_install(LataiRuntime *rt, const char *source, int32_t activate, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc = fail(api->install(rt, source, activate, &p, &n), err);
    take(p, n, out);
    return rc;
}

int32_t lg_activate(LataiRuntime *rt, const char *name, const char *version, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc = fail(api->activate(rt, name, version, &p, &n), err);
    take(p, n, out);
    return rc;
}

int32_t lg_list(LataiRuntime *rt, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc = fail(api->list(rt, &p, &n), err);
    take(p, n, out);
    return rc;
}

int32_t lg_pin(LataiRuntime *rt, const char *name, const char *pin, lg_str *out, lg_str *err) {
    char *p = NULL;
    size_t n = 0;
    int32_t rc = fail(api->pin(rt, name, pin, &p, &n), err);
    take(p, n, out);
    return rc;
}

int32_t lg_stream_open(LataiRuntime *rt, const char *open, size_t len, uintptr_t handle, LataiStream **s, lg_str *err) {
    return fail(api->stream_open(rt, open, len, trampoline, (void *)handle, s), err);
}

int32_t lg_stream_push(LataiStream *s, const uint8_t *data, size_t len, lg_str *err) {
    return fail(api->stream_push(s, data, len), err);
}

int32_t lg_stream_cancel(LataiStream *s, lg_str *err) { return fail(api->stream_cancel(s), err); }

int32_t lg_stream_close(LataiStream *s, int32_t flush, lg_str *err) {
    return fail(api->stream_close(s, flush), err);
}
