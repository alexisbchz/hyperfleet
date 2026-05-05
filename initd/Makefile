BIN := ../bin/hyperfleet-init

# Prefer musl-gcc for a tiny static binary; fall back to gcc -static if musl
# isn't installed. The result is dropped into every guest rootfs at
# /sbin/hyperfleet-init by the daemon at boot time.
CC ?= $(shell command -v musl-gcc 2>/dev/null || command -v gcc)
CFLAGS ?= -O2 -Wall -Wextra -Wno-unused-parameter -static -s

.PHONY: all clean
all: $(BIN)

$(BIN): initd.c
	@mkdir -p $(dir $(BIN))
	$(CC) $(CFLAGS) -o $(BIN) initd.c

clean:
	rm -f $(BIN)
