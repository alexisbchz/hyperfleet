/*
 * hyperfleet-init — in-guest PID 1 for hyperfleet microVMs.
 *
 * Mounts the standard pseudo-filesystems plus a tmpfs at /shared, brings up
 * loopback, and serves a small HTTP control plane over AF_VSOCK so the host
 * daemon can run commands and shuttle tar archives in/out without going
 * through the serial console.
 *
 * Endpoints (all rooted at the vsock listener):
 *   POST /exec    — body: ExecRequest JSON. Response: framed stream.
 *   PUT  /tar     — query: ?path=<abs>. Body: tar archive. 204 on success.
 *   GET  /tar     — query: ?path=<abs>. Body: tar archive of file/dir.
 *   GET  /stat    — query: ?path=<abs>. Body: StatResponse JSON.
 *   GET  /healthz — 204.
 *
 * Frame format on /exec response:
 *   [1 byte kind][4 bytes BE length][N bytes payload]
 *   kind: 1=stdout, 2=stderr, 3=exit (payload=BE int32), 4=error (payload=utf8)
 *
 * Build static against musl:
 *   musl-gcc -O2 -s -static -o bin/hyperfleet-init initd/initd.c
 *
 * Falls back to glibc-static if musl-gcc isn't around; the Makefile target
 * picks whichever is available.
 */

#define _GNU_SOURCE
#include <arpa/inet.h>
#include <ctype.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/reboot.h>
#include <linux/vm_sockets.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/poll.h>
#include <sys/reboot.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#define VSOCK_PORT 1024
#define BACKLOG 16
#define READ_BUF 16384
#define MAX_HEADER (64 * 1024)
#define MAX_REQ_BODY (4 * 1024 * 1024) /* /exec JSON body cap */

#define FRAME_STDOUT 1
#define FRAME_STDERR 2
#define FRAME_EXIT 3
#define FRAME_ERROR 4

#define TTY_CONSOLE_SHELL 1 /* keep /bin/sh on /dev/console for ssh-into-serial */

static void logmsg(const char *fmt, ...) {
    va_list ap;
    fprintf(stderr, "[initd] ");
    va_start(ap, fmt);
    vfprintf(stderr, fmt, ap);
    va_end(ap);
    fputc('\n', stderr);
}

/* ---------- mounts ---------- */

struct mountspec {
    const char *source;
    const char *target;
    const char *fstype;
    unsigned long flags;
    const char *data;
};

static const struct mountspec MOUNTS[] = {
    {"proc", "/proc", "proc", 0, NULL},
    {"sysfs", "/sys", "sysfs", 0, NULL},
    {"devtmpfs", "/dev", "devtmpfs", 0, NULL},
    {"devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666"},
    {"tmpfs", "/tmp", "tmpfs", 0, "mode=1777"},
    {"tmpfs", "/run", "tmpfs", 0, "mode=0755"},
    {"tmpfs", "/shared", "tmpfs", 0, "mode=0755"},
};

static void setup_filesystems(void) {
    for (size_t i = 0; i < sizeof(MOUNTS) / sizeof(MOUNTS[0]); i++) {
        const struct mountspec *m = &MOUNTS[i];
        if (mkdir(m->target, 0755) != 0 && errno != EEXIST) {
            logmsg("mkdir %s: %s", m->target, strerror(errno));
        }
        if (mount(m->source, m->target, m->fstype, m->flags, m->data) != 0) {
            /* EBUSY = already mounted by image's own init; not fatal. */
            if (errno != EBUSY) {
                logmsg("mount %s on %s: %s", m->fstype, m->target, strerror(errno));
            }
        }
    }
}

/* Bring up loopback. eth0 (if present) is configured by the kernel via the
 * ip= boot param the host writes to the cmdline, so nothing to do for it. */
static void setup_loopback(void) {
    /* Use /sbin/ip if present; fall back to ifconfig. Both ship in busybox. */
    pid_t pid = fork();
    if (pid == 0) {
        execl("/sbin/ip", "ip", "link", "set", "lo", "up", (char *)NULL);
        execl("/sbin/ifconfig", "ifconfig", "lo", "up", (char *)NULL);
        _exit(127);
    } else if (pid > 0) {
        int st;
        waitpid(pid, &st, 0);
    }
}

/* ---------- console shell (preserves ssh-into-serial path) ---------- */

#if TTY_CONSOLE_SHELL
static void run_console_shell_loop(void) {
    /* Restart /bin/sh on /dev/console forever so a user typing `exit` doesn't
     * lock the operator out of the SSH gateway during the migration window. */
    for (;;) {
        pid_t pid = fork();
        if (pid == 0) {
            int fd = open("/dev/console", O_RDWR);
            if (fd < 0) _exit(1);
            setsid();
            ioctl(fd, TIOCSCTTY, 0);
            dup2(fd, 0);
            dup2(fd, 1);
            dup2(fd, 2);
            if (fd > 2) close(fd);
            execl("/bin/sh", "sh", (char *)NULL);
            _exit(127);
        } else if (pid > 0) {
            int st;
            waitpid(pid, &st, 0);
        }
        usleep(500 * 1000);
    }
}
#endif

/* ---------- vsock listener ---------- */

static int vsock_listen(uint32_t port) {
    int fd = socket(AF_VSOCK, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (fd < 0) {
        logmsg("socket vsock: %s", strerror(errno));
        return -1;
    }
    struct sockaddr_vm sa;
    memset(&sa, 0, sizeof(sa));
    sa.svm_family = AF_VSOCK;
    sa.svm_port = port;
    sa.svm_cid = VMADDR_CID_ANY;
    if (bind(fd, (struct sockaddr *)&sa, sizeof(sa)) != 0) {
        logmsg("bind vsock: %s", strerror(errno));
        close(fd);
        return -1;
    }
    if (listen(fd, BACKLOG) != 0) {
        logmsg("listen vsock: %s", strerror(errno));
        close(fd);
        return -1;
    }
    return fd;
}

/* ---------- I/O helpers ---------- */

static ssize_t writen(int fd, const void *buf, size_t n) {
    const char *p = buf;
    size_t left = n;
    while (left > 0) {
        ssize_t w = write(fd, p, left);
        if (w < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        if (w == 0) return -1;
        p += w;
        left -= (size_t)w;
    }
    return (ssize_t)n;
}

/* read_until reads from fd into buf (cap N), returning the number of bytes
 * read once `needle` appears in the buffer, or -1 on error / -2 on cap. */
static ssize_t read_until(int fd, char *buf, size_t cap, const char *needle) {
    size_t have = 0;
    size_t nlen = strlen(needle);
    while (have < cap) {
        ssize_t r = read(fd, buf + have, cap - have);
        if (r < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        if (r == 0) return have == 0 ? -1 : (ssize_t)have;
        have += (size_t)r;
        if (have >= nlen) {
            /* memmem isn't standard everywhere; do a tiny scan from the
             * earliest position the needle could end at. */
            for (size_t i = (have >= nlen ? have - (size_t)r - (nlen - 1) : 0);
                 i + nlen <= have; i++) {
                if (memcmp(buf + i, needle, nlen) == 0) {
                    return (ssize_t)have;
                }
            }
        }
    }
    return -2;
}

/* read_exact reads exactly n bytes, blocking. -1 on error/EOF. */
static ssize_t read_exact(int fd, void *buf, size_t n) {
    char *p = buf;
    size_t left = n;
    while (left > 0) {
        ssize_t r = read(fd, p, left);
        if (r < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        if (r == 0) return -1;
        p += r;
        left -= (size_t)r;
    }
    return (ssize_t)n;
}

/* ---------- HTTP request parsing ---------- */

struct request {
    char method[16];
    char path[512];
    char query[512];
    long content_length; /* -1 if absent */
};

/* copy_bounded copies at most cap-1 bytes from src to dst and NUL-terminates.
 * Returns 0 on success, -1 if src is too long for dst. */
static int copy_bounded(char *dst, size_t cap, const char *src) {
    size_t n = strlen(src);
    if (n >= cap) return -1;
    memcpy(dst, src, n);
    dst[n] = 0;
    return 0;
}

static int parse_request_line(char *line, struct request *req) {
    char *sp1 = strchr(line, ' ');
    if (!sp1) return -1;
    *sp1 = 0;
    char *sp2 = strchr(sp1 + 1, ' ');
    if (!sp2) return -1;
    *sp2 = 0;
    if (copy_bounded(req->method, sizeof(req->method), line) != 0) return -1;
    char *url = sp1 + 1;
    char *q = strchr(url, '?');
    if (q) {
        *q = 0;
        if (copy_bounded(req->query, sizeof(req->query), q + 1) != 0) return -1;
    } else {
        req->query[0] = 0;
    }
    if (copy_bounded(req->path, sizeof(req->path), url) != 0) return -1;
    return 0;
}

/* hex_decode in place; returns new length. Caller-owned buf. */
static size_t url_decode(char *s) {
    char *r = s, *w = s;
    while (*r) {
        if (*r == '%' && isxdigit((unsigned char)r[1]) && isxdigit((unsigned char)r[2])) {
            char hex[3] = {r[1], r[2], 0};
            *w++ = (char)strtol(hex, NULL, 16);
            r += 3;
        } else if (*r == '+') {
            *w++ = ' ';
            r++;
        } else {
            *w++ = *r++;
        }
    }
    *w = 0;
    return (size_t)(w - s);
}

static int query_get(const char *query, const char *key, char *out, size_t cap) {
    size_t klen = strlen(key);
    const char *p = query;
    while (*p) {
        if (strncmp(p, key, klen) == 0 && p[klen] == '=') {
            const char *v = p + klen + 1;
            const char *end = strchr(v, '&');
            size_t n = end ? (size_t)(end - v) : strlen(v);
            if (n >= cap) return -1;
            memcpy(out, v, n);
            out[n] = 0;
            url_decode(out);
            return 0;
        }
        const char *amp = strchr(p, '&');
        if (!amp) break;
        p = amp + 1;
    }
    return -1;
}

/* parse_headers walks header bytes (after request line, before \r\n\r\n) and
 * fills content_length. Other headers ignored in v0. Buffer is mutated. */
static void parse_headers(char *headers, struct request *req) {
    req->content_length = -1;
    char *line = headers;
    while (line && *line) {
        char *eol = strstr(line, "\r\n");
        if (!eol) break;
        *eol = 0;
        if (strncasecmp(line, "Content-Length:", 15) == 0) {
            const char *v = line + 15;
            while (*v == ' ' || *v == '\t') v++;
            req->content_length = strtol(v, NULL, 10);
        }
        line = eol + 2;
    }
}

/* ---------- response helpers ---------- */

static void respond_simple(int fd, int status, const char *reason,
                           const char *content_type, const char *body) {
    char hdr[512];
    size_t blen = body ? strlen(body) : 0;
    int n = snprintf(hdr, sizeof(hdr),
                     "HTTP/1.1 %d %s\r\n"
                     "Content-Type: %s\r\n"
                     "Content-Length: %zu\r\n"
                     "Connection: close\r\n\r\n",
                     status, reason, content_type ? content_type : "text/plain", blen);
    if (n > 0) writen(fd, hdr, (size_t)n);
    if (blen) writen(fd, body, blen);
}

static void respond_204(int fd) {
    static const char r[] =
        "HTTP/1.1 204 No Content\r\nConnection: close\r\nContent-Length: 0\r\n\r\n";
    writen(fd, r, sizeof(r) - 1);
}

static void respond_chunked_start(int fd, int status, const char *content_type) {
    char hdr[256];
    int n = snprintf(hdr, sizeof(hdr),
                     "HTTP/1.1 %d OK\r\n"
                     "Content-Type: %s\r\n"
                     "Transfer-Encoding: chunked\r\n"
                     "Connection: close\r\n\r\n",
                     status, content_type);
    if (n > 0) writen(fd, hdr, (size_t)n);
}

/* write a single chunked HTTP body chunk. */
static int chunked_write(int fd, const void *data, size_t len) {
    char hdr[32];
    int n = snprintf(hdr, sizeof(hdr), "%zx\r\n", len);
    if (n <= 0 || writen(fd, hdr, (size_t)n) < 0) return -1;
    if (len && writen(fd, data, len) < 0) return -1;
    if (writen(fd, "\r\n", 2) < 0) return -1;
    return 0;
}

static int chunked_end(int fd) {
    return writen(fd, "0\r\n\r\n", 5) < 0 ? -1 : 0;
}

/* ---------- /exec ---------- */

/* Minimal JSON walker over the request body. We accept the schema:
 *   {"command":["..","..."], "env":{"K":"V",...}, "workdir":"...", "user":"..."}
 *
 * Strict enough for the runner; not a general JSON parser.
 *
 * Returns 0 on success; on success, *cmd/cmd_count point to a heap argv,
 * *env points to a NULL-terminated heap envp entries (unowned strings live
 * in `body`, which the caller keeps alive for the lifetime of cmd/env).
 */
struct exec_req {
    char **argv;
    int argc;
    char **envp;          /* entries are "K=V"; NULL-terminated */
    int env_count;
    const char *workdir;  /* NULL or absolute path */
};

static char *json_skip_ws(char *p) {
    while (*p && (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')) p++;
    return p;
}

/* Parses a JSON string starting at *pp (must point at the opening quote).
 * Decodes \n \t \r \" \\ \/ \b \f and \uXXXX escapes in place; the latter
 * encodes to UTF-8 (BMP only — surrogate pairs are emitted as the U+FFFD
 * replacement so we never write invalid UTF-8). Advances *pp past the
 * closing quote and returns a pointer to the (NUL-terminated) string. */
static int hex_nibble(char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static char *json_parse_string(char **pp) {
    char *p = *pp;
    if (*p != '"') return NULL;
    p++;
    char *start = p;
    char *w = p;
    while (*p && *p != '"') {
        if (*p == '\\' && p[1]) {
            switch (p[1]) {
                case 'n': *w++ = '\n'; p += 2; break;
                case 't': *w++ = '\t'; p += 2; break;
                case 'r': *w++ = '\r'; p += 2; break;
                case 'b': *w++ = '\b'; p += 2; break;
                case 'f': *w++ = '\f'; p += 2; break;
                case '"': *w++ = '"';  p += 2; break;
                case '\\': *w++ = '\\'; p += 2; break;
                case '/': *w++ = '/';  p += 2; break;
                case 'u': {
                    int a = p[2] ? hex_nibble(p[2]) : -1;
                    int b = p[3] ? hex_nibble(p[3]) : -1;
                    int c = p[4] ? hex_nibble(p[4]) : -1;
                    int d = p[5] ? hex_nibble(p[5]) : -1;
                    if (a < 0 || b < 0 || c < 0 || d < 0) return NULL;
                    unsigned cp = (unsigned)((a << 12) | (b << 8) | (c << 4) | d);
                    if (cp >= 0xD800 && cp <= 0xDFFF) {
                        /* Lone surrogate; emit U+FFFD. Surrogate pairs would
                         * need a second \uXXXX read which v0 skips. */
                        *w++ = (char)0xEF;
                        *w++ = (char)0xBF;
                        *w++ = (char)0xBD;
                    } else if (cp < 0x80) {
                        *w++ = (char)cp;
                    } else if (cp < 0x800) {
                        *w++ = (char)(0xC0 | (cp >> 6));
                        *w++ = (char)(0x80 | (cp & 0x3F));
                    } else {
                        *w++ = (char)(0xE0 | (cp >> 12));
                        *w++ = (char)(0x80 | ((cp >> 6) & 0x3F));
                        *w++ = (char)(0x80 | (cp & 0x3F));
                    }
                    p += 6;
                    break;
                }
                default:
                    *w++ = p[1];
                    p += 2;
                    break;
            }
        } else {
            *w++ = *p++;
        }
    }
    if (*p != '"') return NULL;
    *w = 0;
    *pp = p + 1;
    return start;
}

static int parse_exec_json(char *body, struct exec_req *out) {
    memset(out, 0, sizeof(*out));
    char *p = json_skip_ws(body);
    if (*p != '{') return -1;
    p++;
    while (*p) {
        p = json_skip_ws(p);
        if (*p == '}') { p++; break; }
        char *key = json_parse_string(&p);
        if (!key) return -1;
        p = json_skip_ws(p);
        if (*p != ':') return -1;
        p++;
        p = json_skip_ws(p);

        if (strcmp(key, "command") == 0) {
            if (*p != '[') return -1;
            p++;
            /* count entries first; we'll allocate argv accordingly. */
            int cap = 8;
            out->argv = calloc((size_t)cap + 1, sizeof(char *));
            if (!out->argv) return -1;
            for (;;) {
                p = json_skip_ws(p);
                if (*p == ']') { p++; break; }
                char *s = json_parse_string(&p);
                if (!s) return -1;
                if (out->argc >= cap) {
                    cap *= 2;
                    char **g = realloc(out->argv, ((size_t)cap + 1) * sizeof(char *));
                    if (!g) return -1;
                    out->argv = g;
                }
                out->argv[out->argc++] = s;
                p = json_skip_ws(p);
                if (*p == ',') p++;
            }
            out->argv[out->argc] = NULL;
        } else if (strcmp(key, "env") == 0) {
            if (*p != '{') return -1;
            p++;
            int cap = 8;
            out->envp = calloc((size_t)cap + 1, sizeof(char *));
            if (!out->envp) return -1;
            for (;;) {
                p = json_skip_ws(p);
                if (*p == '}') { p++; break; }
                char *k = json_parse_string(&p);
                if (!k) return -1;
                p = json_skip_ws(p);
                if (*p != ':') return -1;
                p++;
                p = json_skip_ws(p);
                char *v = json_parse_string(&p);
                if (!v) return -1;
                size_t klen = strlen(k), vlen = strlen(v);
                char *kv = malloc(klen + 1 + vlen + 1);
                if (!kv) return -1;
                memcpy(kv, k, klen);
                kv[klen] = '=';
                memcpy(kv + klen + 1, v, vlen);
                kv[klen + 1 + vlen] = 0;
                if (out->env_count >= cap) {
                    cap *= 2;
                    char **g = realloc(out->envp, ((size_t)cap + 1) * sizeof(char *));
                    if (!g) return -1;
                    out->envp = g;
                }
                out->envp[out->env_count++] = kv;
                p = json_skip_ws(p);
                if (*p == ',') p++;
            }
            if (out->envp) out->envp[out->env_count] = NULL;
        } else if (strcmp(key, "workdir") == 0) {
            char *s = json_parse_string(&p);
            if (!s) return -1;
            out->workdir = s;
        } else if (strcmp(key, "user") == 0) {
            char *s = json_parse_string(&p);
            if (!s) return -1;
            (void)s; /* reserved */
        } else {
            /* Unknown key: skip the value. v0 is strict-enough; full skipping
             * would need a real parser. Reject unknown keys for now. */
            return -1;
        }
        p = json_skip_ws(p);
        if (*p == ',') p++;
    }
    if (out->argc == 0) return -1;
    return 0;
}

/* Build envp by overlaying req->envp on the current process environment.
 * Returned array (and its strings, where copied) are heap; caller frees with
 * free_envp(). */
extern char **environ;

static void free_envp(char **envp) {
    if (!envp) return;
    for (int i = 0; envp[i]; i++) free(envp[i]);
    free(envp);
}

static char **build_envp(char **overlay, int overlay_count) {
    int base_count = 0;
    while (environ[base_count]) base_count++;
    char **out = calloc((size_t)(base_count + overlay_count + 1), sizeof(char *));
    if (!out) return NULL;
    int n = 0;
    for (int i = 0; i < base_count; i++) {
        const char *eq = strchr(environ[i], '=');
        if (!eq) continue;
        size_t klen = (size_t)(eq - environ[i]);
        int overridden = 0;
        for (int j = 0; j < overlay_count; j++) {
            const char *oeq = strchr(overlay[j], '=');
            if (!oeq) continue;
            if ((size_t)(oeq - overlay[j]) == klen &&
                memcmp(overlay[j], environ[i], klen) == 0) {
                overridden = 1;
                break;
            }
        }
        if (!overridden) out[n++] = strdup(environ[i]);
    }
    for (int j = 0; j < overlay_count; j++) out[n++] = strdup(overlay[j]);
    out[n] = NULL;
    return out;
}

static void handle_exec(int fd, long content_length, char *prefetched, size_t prefetched_len) {
    if (content_length <= 0 || content_length > MAX_REQ_BODY) {
        respond_simple(fd, 400, "Bad Request", "text/plain", "content-length required\n");
        return;
    }
    char *body = malloc((size_t)content_length + 1);
    if (!body) {
        respond_simple(fd, 500, "Internal Error", "text/plain", "oom\n");
        return;
    }
    size_t have = prefetched_len;
    if (have > (size_t)content_length) have = (size_t)content_length;
    memcpy(body, prefetched, have);
    if (have < (size_t)content_length) {
        ssize_t r = read_exact(fd, body + have, (size_t)content_length - have);
        if (r < 0) {
            free(body);
            return;
        }
    }
    body[content_length] = 0;

    struct exec_req req;
    if (parse_exec_json(body, &req) != 0) {
        respond_simple(fd, 400, "Bad Request", "text/plain", "bad exec body\n");
        free(body);
        return;
    }

    respond_chunked_start(fd, 200, "application/octet-stream");

    if (req.workdir && req.workdir[0]) {
        if (mkdir(req.workdir, 0755) != 0 && errno != EEXIST) {
            char msg[256];
            snprintf(msg, sizeof(msg), "mkdir workdir: %s", strerror(errno));
            uint8_t fhdr[5];
            fhdr[0] = FRAME_ERROR;
            uint32_t be = htonl((uint32_t)strlen(msg));
            memcpy(fhdr + 1, &be, 4);
            chunked_write(fd, fhdr, 5);
            chunked_write(fd, msg, strlen(msg));
            chunked_end(fd);
            free(body);
            free(req.argv);
            free_envp(req.envp);
            return;
        }
    }

    int outpipe[2], errpipe[2];
    if (pipe2(outpipe, O_CLOEXEC) != 0 || pipe2(errpipe, O_CLOEXEC) != 0) {
        chunked_end(fd);
        free(body);
        free(req.argv);
        free_envp(req.envp);
        return;
    }

    char **envp = build_envp(req.envp, req.env_count);

    pid_t pid = fork();
    if (pid == 0) {
        close(outpipe[0]);
        close(errpipe[0]);
        dup2(outpipe[1], 1);
        dup2(errpipe[1], 2);
        close(outpipe[1]);
        close(errpipe[1]);
        if (req.workdir && req.workdir[0]) {
            if (chdir(req.workdir) != 0) {
                fprintf(stderr, "chdir %s: %s\n", req.workdir, strerror(errno));
                _exit(127);
            }
        }
        execvpe(req.argv[0], req.argv, envp ? envp : environ);
        fprintf(stderr, "exec %s: %s\n", req.argv[0], strerror(errno));
        _exit(127);
    }
    if (pid < 0) {
        chunked_end(fd);
        close(outpipe[0]); close(outpipe[1]);
        close(errpipe[0]); close(errpipe[1]);
        free_envp(envp);
        free(body);
        free(req.argv);
        free_envp(req.envp);
        return;
    }

    close(outpipe[1]);
    close(errpipe[1]);
    free_envp(envp);

    /* Pump both pipes concurrently, frame each chunk into the chunked body. */
    struct pollfd pfds[2];
    pfds[0].fd = outpipe[0]; pfds[0].events = POLLIN;
    pfds[1].fd = errpipe[0]; pfds[1].events = POLLIN;
    int open_count = 2;
    char buf[READ_BUF];
    while (open_count > 0) {
        int pn = poll(pfds, 2, -1);
        if (pn < 0) {
            if (errno == EINTR) continue;
            break;
        }
        for (int i = 0; i < 2; i++) {
            if (pfds[i].fd < 0) continue;
            if (pfds[i].revents & (POLLIN | POLLHUP | POLLERR)) {
                ssize_t r = read(pfds[i].fd, buf, sizeof(buf));
                if (r > 0) {
                    uint8_t fhdr[5];
                    fhdr[0] = (i == 0) ? FRAME_STDOUT : FRAME_STDERR;
                    uint32_t be = htonl((uint32_t)r);
                    memcpy(fhdr + 1, &be, 4);
                    /* one chunked frame per pipe read */
                    char framed[5 + READ_BUF];
                    memcpy(framed, fhdr, 5);
                    memcpy(framed + 5, buf, (size_t)r);
                    if (chunked_write(fd, framed, 5 + (size_t)r) != 0) {
                        /* client gone; reap and bail */
                        kill(pid, SIGTERM);
                        close(outpipe[0]);
                        close(errpipe[0]);
                        waitpid(pid, NULL, 0);
                        free(body);
                        free(req.argv);
                        free_envp(req.envp);
                        return;
                    }
                } else if (r == 0 || (r < 0 && errno != EAGAIN && errno != EINTR)) {
                    close(pfds[i].fd);
                    pfds[i].fd = -1;
                    open_count--;
                }
            }
        }
    }

    int wstatus = 0;
    waitpid(pid, &wstatus, 0);

    uint8_t fhdr[5];
    if (WIFEXITED(wstatus)) {
        uint32_t code = htonl((uint32_t)(int32_t)WEXITSTATUS(wstatus));
        fhdr[0] = FRAME_EXIT;
        uint32_t be = htonl(4);
        memcpy(fhdr + 1, &be, 4);
        char framed[5 + 4];
        memcpy(framed, fhdr, 5);
        memcpy(framed + 5, &code, 4);
        chunked_write(fd, framed, sizeof(framed));
    } else if (WIFSIGNALED(wstatus)) {
        char msg[64];
        int n = snprintf(msg, sizeof(msg), "killed by signal %d", WTERMSIG(wstatus));
        fhdr[0] = FRAME_ERROR;
        uint32_t be = htonl((uint32_t)n);
        memcpy(fhdr + 1, &be, 4);
        char framed[5 + 64];
        memcpy(framed, fhdr, 5);
        memcpy(framed + 5, msg, (size_t)n);
        chunked_write(fd, framed, 5 + (size_t)n);
    }
    chunked_end(fd);

    free(body);
    free(req.argv);
    free_envp(req.envp);
}

/* ---------- /tar (PUT extract / GET stream) ---------- */

static int spawn_tar_extract(const char *path, int *stdin_fd_out, pid_t *pid_out) {
    int p[2];
    if (pipe2(p, O_CLOEXEC) != 0) return -1;
    pid_t pid = fork();
    if (pid == 0) {
        close(p[1]);
        dup2(p[0], 0);
        close(p[0]);
        execlp("tar", "tar", "-xf", "-", "-C", path, (char *)NULL);
        _exit(127);
    }
    if (pid < 0) {
        close(p[0]); close(p[1]);
        return -1;
    }
    close(p[0]);
    *stdin_fd_out = p[1];
    *pid_out = pid;
    return 0;
}

static int spawn_tar_create(const char *path, int *stdout_fd_out, pid_t *pid_out) {
    int p[2];
    if (pipe2(p, O_CLOEXEC) != 0) return -1;
    pid_t pid = fork();
    if (pid == 0) {
        close(p[0]);
        dup2(p[1], 1);
        close(p[1]);
        struct stat st;
        if (stat(path, &st) != 0) _exit(1);
        if (S_ISDIR(st.st_mode)) {
            execlp("tar", "tar", "-cf", "-", "-C", path, ".", (char *)NULL);
        } else {
            char *dup = strdup(path);
            char *base = strrchr(dup, '/');
            const char *dir = "/";
            const char *file = path;
            if (base) {
                if (base == dup) {
                    dir = "/";
                    file = base + 1;
                } else {
                    *base = 0;
                    dir = dup;
                    file = base + 1;
                }
            }
            execlp("tar", "tar", "-cf", "-", "-C", dir, file, (char *)NULL);
        }
        _exit(127);
    }
    if (pid < 0) {
        close(p[0]); close(p[1]);
        return -1;
    }
    close(p[1]);
    *stdout_fd_out = p[0];
    *pid_out = pid;
    return 0;
}

static void handle_tar_put(int fd, const char *path, long content_length,
                           char *prefetched, size_t prefetched_len) {
    if (content_length < 0) {
        respond_simple(fd, 400, "Bad Request", "text/plain", "content-length required\n");
        return;
    }
    if (mkdir(path, 0755) != 0 && errno != EEXIST) {
        respond_simple(fd, 500, "Internal Error", "text/plain", strerror(errno));
        return;
    }

    int tar_in;
    pid_t tar_pid;
    if (spawn_tar_extract(path, &tar_in, &tar_pid) != 0) {
        respond_simple(fd, 500, "Internal Error", "text/plain", "spawn tar\n");
        return;
    }

    /* Forward already-buffered body bytes first. */
    size_t prefetched_to_use = prefetched_len > (size_t)content_length
                                 ? (size_t)content_length
                                 : prefetched_len;
    if (prefetched_to_use > 0) writen(tar_in, prefetched, prefetched_to_use);
    long left = content_length - (long)prefetched_to_use;
    char buf[READ_BUF];
    while (left > 0) {
        ssize_t r = read(fd, buf, left > (long)sizeof(buf) ? sizeof(buf) : (size_t)left);
        if (r <= 0) break;
        if (writen(tar_in, buf, (size_t)r) < 0) break;
        left -= r;
    }
    close(tar_in);
    int st;
    waitpid(tar_pid, &st, 0);
    if (!WIFEXITED(st) || WEXITSTATUS(st) != 0) {
        respond_simple(fd, 500, "Internal Error", "text/plain", "tar extract failed\n");
        return;
    }
    respond_204(fd);
}

static void handle_tar_get(int fd, const char *path) {
    int tar_out;
    pid_t tar_pid;
    if (spawn_tar_create(path, &tar_out, &tar_pid) != 0) {
        respond_simple(fd, 500, "Internal Error", "text/plain", "spawn tar\n");
        return;
    }
    respond_chunked_start(fd, 200, "application/x-tar");
    char buf[READ_BUF];
    for (;;) {
        ssize_t r = read(tar_out, buf, sizeof(buf));
        if (r <= 0) break;
        if (chunked_write(fd, buf, (size_t)r) != 0) break;
    }
    close(tar_out);
    int st;
    waitpid(tar_pid, &st, 0);
    chunked_end(fd);
}

/* ---------- /stat ---------- */

static void handle_stat(int fd, const char *path) {
    struct stat st;
    int ok = (stat(path, &st) == 0);
    char body[256];
    int n;
    if (ok) {
        n = snprintf(body, sizeof(body),
                     "{\"exists\":true,\"isDir\":%s,\"mode\":%u,\"size\":%lld}\n",
                     S_ISDIR(st.st_mode) ? "true" : "false",
                     (unsigned)st.st_mode,
                     (long long)st.st_size);
    } else {
        n = snprintf(body, sizeof(body), "{\"exists\":false}\n");
    }
    char hdr[256];
    int hn = snprintf(hdr, sizeof(hdr),
                      "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
                      "Content-Length: %d\r\nConnection: close\r\n\r\n", n);
    if (hn > 0) writen(fd, hdr, (size_t)hn);
    writen(fd, body, (size_t)n);
}

/* ---------- per-connection dispatch ---------- */

static void handle_connection(int fd) {
    char buf[MAX_HEADER + 1];
    ssize_t total = read_until(fd, buf, MAX_HEADER, "\r\n\r\n");
    if (total <= 0) {
        close(fd);
        return;
    }
    buf[total] = 0;

    char *headers_end = strstr(buf, "\r\n\r\n");
    if (!headers_end) {
        close(fd);
        return;
    }
    *headers_end = 0;
    size_t header_bytes = (size_t)(headers_end - buf) + 4;
    char *body_prefetch = buf + header_bytes;
    size_t body_prefetch_len = (size_t)total - header_bytes;

    char *reqline_end = strstr(buf, "\r\n");
    if (!reqline_end) {
        close(fd);
        return;
    }
    *reqline_end = 0;

    struct request req;
    if (parse_request_line(buf, &req) != 0) {
        respond_simple(fd, 400, "Bad Request", "text/plain", "bad request line\n");
        close(fd);
        return;
    }
    parse_headers(reqline_end + 2, &req);

    if (strcmp(req.path, "/exec") == 0) {
        if (strcmp(req.method, "POST") != 0) {
            respond_simple(fd, 405, "Method Not Allowed", "text/plain", "POST required\n");
        } else {
            handle_exec(fd, req.content_length, body_prefetch, body_prefetch_len);
        }
    } else if (strcmp(req.path, "/tar") == 0) {
        char path[512];
        if (query_get(req.query, "path", path, sizeof(path)) != 0 || path[0] != '/') {
            respond_simple(fd, 400, "Bad Request", "text/plain", "abs path query required\n");
        } else if (strcmp(req.method, "PUT") == 0) {
            handle_tar_put(fd, path, req.content_length, body_prefetch, body_prefetch_len);
        } else if (strcmp(req.method, "GET") == 0) {
            handle_tar_get(fd, path);
        } else {
            respond_simple(fd, 405, "Method Not Allowed", "text/plain", "GET or PUT\n");
        }
    } else if (strcmp(req.path, "/stat") == 0) {
        char path[512];
        if (query_get(req.query, "path", path, sizeof(path)) != 0 || path[0] != '/') {
            respond_simple(fd, 400, "Bad Request", "text/plain", "abs path query required\n");
        } else if (strcmp(req.method, "GET") == 0) {
            handle_stat(fd, path);
        } else {
            respond_simple(fd, 405, "Method Not Allowed", "text/plain", "GET\n");
        }
    } else if (strcmp(req.path, "/healthz") == 0) {
        respond_204(fd);
    } else {
        respond_simple(fd, 404, "Not Found", "text/plain", "not found\n");
    }

    close(fd);
}

/* SIGCHLD handler is a no-op; we explicitly waitpid() on every fork()
 * elsewhere. But we still need to install a handler so the kernel doesn't
 * accumulate zombies for any straggler child the code path missed. */
static void sigchld_reaper(int sig) {
    (void)sig;
    while (waitpid(-1, NULL, WNOHANG) > 0) {
    }
}

int main(void) {
    if (getpid() != 1) {
        fprintf(stderr, "initd: refusing to run as non-PID-1\n");
        return 1;
    }
    logmsg("hyperfleet init starting");

    /* Ignore SIGPIPE: we write to TCP/vsock and child pipes; broken peers
     * shouldn't kill PID 1. */
    signal(SIGPIPE, SIG_IGN);
    struct sigaction sa = {0};
    sa.sa_handler = sigchld_reaper;
    sa.sa_flags = SA_NOCLDSTOP | SA_RESTART;
    sigaction(SIGCHLD, &sa, NULL);

    setup_filesystems();
    setup_loopback();

#if TTY_CONSOLE_SHELL
    if (fork() == 0) {
        run_console_shell_loop();
        _exit(0);
    }
#endif

    int lfd = vsock_listen(VSOCK_PORT);
    if (lfd < 0) {
        logmsg("could not listen on vsock; halting");
        sleep(2);
        reboot(LINUX_REBOOT_CMD_POWER_OFF);
        return 1;
    }
    logmsg("listening on vsock port %u", VSOCK_PORT);

    for (;;) {
        struct sockaddr_vm peer;
        socklen_t plen = sizeof(peer);
        int fd = accept4(lfd, (struct sockaddr *)&peer, &plen, SOCK_CLOEXEC);
        if (fd < 0) {
            if (errno == EINTR) continue;
            logmsg("accept: %s", strerror(errno));
            continue;
        }
        /* Fork a child per connection. PID 1 stays in the accept loop; the
         * SIGCHLD handler will reap. Concurrency over a single VM is bounded
         * by the runner (one Exec at a time per env in practice). */
        pid_t pid = fork();
        if (pid == 0) {
            close(lfd);
            handle_connection(fd);
            _exit(0);
        }
        close(fd);
    }
}
