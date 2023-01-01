FROM golang:1.18

WORKDIR /kill-gpu-zombie-pod
COPY . .
RUN go build -o kill_gpu_zombie_pod

CMD ["kill_gpu_zombie_pod -namespace=default -idle_timeout_seconds=90 -check_period_seconds=10"]