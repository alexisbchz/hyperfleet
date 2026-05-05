.PHONY: install-firecracker install-containerd setup-devmapper setup kernel bootstrap run stop tidy build fleet initd plugin

install-firecracker:
	./scripts/install-firecracker.sh

install-containerd:
	./scripts/install-containerd.sh

setup-devmapper:
	sudo install -m 0755 ./scripts/setup-devmapper.sh /usr/local/sbin/hyperfleet-setup-devmapper
	sudo install -m 0644 ./scripts/hyperfleet-devmapper.service /etc/systemd/system/hyperfleet-devmapper.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now hyperfleet-devmapper.service

setup:
	./scripts/setup.sh

kernel:
	./scripts/download-kernel.sh

bootstrap: install-containerd setup-devmapper install-firecracker kernel

tidy:
	go mod tidy

run:
	FIRECRACKER_BIN=./bin/firecracker \
	KERNEL_PATH=./assets/vmlinux \
	go run ./cmd/serve

stop:
	./scripts/stop.sh

build:
	go build -o bin/serve ./cmd/serve
	go build -o bin/fleet ./cmd/fleet

fleet:
	go build -o bin/fleet ./cmd/fleet

initd:
	$(MAKE) -C initd

plugin:
	cd forgejo-plugin && go build -o ../bin/hyperfleet-forgejo-plugin .
