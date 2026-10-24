/* SPDX-License-Identifier: Apache-2.0 OR MIT */
/* The C side of the Go SDK: load liblatai at run time and call its table (include/latai.h).
   Every call that can fail copies the thread's last_error before it returns, so the Go side
   never needs the OS thread the call ran on. */
#ifndef LATAI_GO_SHIM_H
#define LATAI_GO_SHIM_H

#include <stddef.h>
#include <stdint.h>
#include "latai.h"

/* A string handed to Go: malloc'd by the shim (NULL when there is none), released with lg_free. */
typedef struct {
    char *ptr;
    size_t len;
} lg_str;

/* Load the library at `path` and take the table of `version`. 0 on success; else a message in
   `*err` (to lg_free). Loading twice keeps the first library. */
int lg_load(const char *path, uint32_t version, lg_str *err);
uint32_t lg_version(void);
void lg_free(void *p);

int32_t lg_open(const char *cfg, size_t len, LataiRuntime **rt, lg_str *err);
int32_t lg_close(LataiRuntime *rt, lg_str *err);
int32_t lg_info(LataiRuntime *rt, lg_str *out, lg_str *err);
int32_t lg_run(LataiRuntime *rt, const char *req, size_t len, const char *id, lg_str *out, lg_str *err);
int32_t lg_run_stream(LataiRuntime *rt, const char *req, size_t len, const char *id, uintptr_t handle, lg_str *err);
int32_t lg_cancel(LataiRuntime *rt, const char *id, lg_str *err);
int32_t lg_session_close(LataiRuntime *rt, const char *session, lg_str *err);

/* The pack calls that answer one string: 0 inspect, 1 stage, 2 rollback, 3 status. */
int32_t lg_call1(int which, LataiRuntime *rt, const char *arg, lg_str *out, lg_str *err);
int32_t lg_pull(LataiRuntime *rt, const char *url, int32_t force, lg_str *out, lg_str *err);
int32_t lg_install(LataiRuntime *rt, const char *source, int32_t activate, lg_str *out, lg_str *err);
int32_t lg_activate(LataiRuntime *rt, const char *name, const char *version, lg_str *out, lg_str *err);
int32_t lg_list(LataiRuntime *rt, lg_str *out, lg_str *err);
int32_t lg_pin(LataiRuntime *rt, const char *name, const char *pin, lg_str *out, lg_str *err);

int32_t lg_stream_open(LataiRuntime *rt, const char *open, size_t len, uintptr_t handle, LataiStream **s, lg_str *err);
int32_t lg_stream_push(LataiStream *s, const uint8_t *data, size_t len, lg_str *err);
int32_t lg_stream_cancel(LataiStream *s, lg_str *err);
int32_t lg_stream_close(LataiStream *s, int32_t flush, lg_str *err);

#endif
