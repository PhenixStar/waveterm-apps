.PHONY: all dashboard docker-panel clean

all: dashboard docker-panel

dashboard:
	go build -o bin/wt-dashboard.exe ./cmd/dashboard/

docker-panel:
	go build -o bin/wt-docker-panel.exe ./cmd/docker-panel/

clean:
	rm -rf bin/
