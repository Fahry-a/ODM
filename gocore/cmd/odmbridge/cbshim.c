#include "cbshim.h"
#include <stdio.h>
#include <stddef.h>
#include <time.h>
#include <errno.h>
#include <stdint.h>
#ifdef __linux__
#include <sys/syscall.h>
#include <unistd.h>
#include <sys/time.h>
#endif

#ifdef __linux__
/* 32-bit userland on a 64-bit ARMv8 kernel (e.g. 32-bit Termux) can make
 * musl's clock_gettime return EOVERFLOW. Call clock_gettime64 directly and
 * narrow to 32-bit time_t. Only affects 32-bit Linux builds. */
#ifndef __NR_clock_gettime64
#define __NR_clock_gettime64 403
#endif
#if defined(__arm__) || defined(__i386__)
struct odm_ts64 { int64_t tv_sec; int64_t tv_nsec; };
extern int __real_clock_gettime(clockid_t id, struct timespec *tp);

int __wrap_clock_gettime(clockid_t id, struct timespec *tp) {
    struct odm_ts64 t;
    /* Android 32-bit compat may not implement clock_gettime64 at all. */
    if (syscall(__NR_clock_gettime64, id, &t) == 0) {
        if (t.tv_sec > INT32_MAX || t.tv_sec < INT32_MIN) {
            errno = EOVERFLOW;
            return -1;
        }
        tp->tv_sec = (time_t)t.tv_sec;
        tp->tv_nsec = (long)t.tv_nsec;
        return 0;
    }
    if (__real_clock_gettime(id, tp) == 0) return 0;
#ifdef CLOCK_MONOTONIC_COARSE
    if (id == CLOCK_MONOTONIC && __real_clock_gettime(CLOCK_MONOTONIC_COARSE, tp) == 0) return 0;
#endif
    /* Last resort: wall clock (fits 32-bit time_t until 2038). */
    struct timeval tv;
    if (gettimeofday(&tv, 0) == 0) {
        tp->tv_sec = (time_t)tv.tv_sec;
        tp->tv_nsec = (long)tv.tv_usec * 1000;
        return 0;
    }
    return -1;
}
#endif

/* 32-bit musl/zig libc lacks this helper used by vendored OpenSSL. */
#if defined(__i386__) || defined(__arm__)
_Bool odm_atomic_is_lock_free(size_t size, const volatile void *ptr) { (void)ptr; return size <= 4; }
__asm__(".globl __atomic_is_lock_free\n.set __atomic_is_lock_free, odm_atomic_is_lock_free");
#endif
#endif /* __linux__ */

/*
 * The callback lives in a real C translation unit (not the cgo preamble):
 * cgo copies a //export file's preamble into two outputs, so a preamble-local
 * static would give the setter and the emitter different copies.
 */
static odm_progress_cb odm_progress_cb_store;

void odm_shim_set_progress(odm_progress_cb cb) {
    odm_progress_cb_store = cb;
}

void odm_shim_progress(const char *event_json) {
    odm_progress_cb cb = odm_progress_cb_store;
    if (cb != 0 && event_json != 0) {
        cb(event_json);
    }
}

/*
 * musl invokes .init_array constructors with no arguments, while Go's
 * c-archive runtime constructor expects (argc, argv, envp). The post-link
 * patcher points the Go constructor slot at odm_go_ctor_shim and stores the
 * original constructor address in odm_go_ctor_ptr.
 */
extern char **environ;

/* Non-zero init forces .data (not .bss) so the patcher has file bytes to write. */
void (*odm_go_ctor_ptr)(int, char **, char **) = (void (*)(int, char **, char **))1;

static int odm_argc_from_proc(void) {
    FILE *f = fopen("/proc/self/cmdline", "rb");
    if (f == 0) return 1;
    int c, prev = 1, n = 0;
    while ((c = fgetc(f)) != EOF) {
        if (c == 0 && prev != 0) ++n;
        prev = c;
    }
    fclose(f);
    return n > 0 ? n : 1;
}

void odm_go_ctor_shim(void) {
    int argc = odm_argc_from_proc();
    char **argv = environ - (argc + 1);
    if (odm_go_ctor_ptr != 0) {
        odm_go_ctor_ptr(argc, argv, environ);
    }
}
