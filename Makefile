
build:
	go build -o sysweaver cmd/main.go
	mv sysweaver bin/sysweaver

build-test:
	go build -o sysweaver cmd/main.go
	mv sysweaver test/sysweaver

run: 
	echo -e "\n=== Run ===\n"

	bin/sysweaver

build-run: build run
