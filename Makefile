.PHONY: all dashboard dashboard-web docker-panel docker-panel-web monitor clean

all: dashboard dashboard-web docker-panel docker-panel-web monitor

dashboard:
	go build -o bin/wt-dashboard.exe ./cmd/dashboard/

dashboard-web:
	go build -o bin/wt-dashboard-web.exe ./cmd/dashboard-web/

docker-panel:
	go build -o bin/wt-docker-panel.exe ./cmd/docker-panel/

docker-panel-web:
	go build -o bin/wt-docker-panel-web.exe ./cmd/docker-panel-web/

monitor:
	go build -o bin/wt-monitor.exe ./cmd/monitor/

clean:
	rm -rf bin/
