
build:
	go build -o sysweaver cmd/main.go
	mv sysweaver bin/sysweaver

build-test:
	go build -o sysweaver cmd/main.go
	mv sysweaver test/sysweaver

run: 
	echo -e "\n=== Run ===\n"

	bin/sysweaver

clr-out:
	rm -rf output/*

build-run: build run
