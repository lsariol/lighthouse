#!/bin/sh
# Runs TestRealSelfUpdate in a container of the Lighthouse image, with the Docker socket; run from the top.
set -eu

base=lighthouse-e2e-base
mkdir -p .dev/e2e
docker build -q -t "$base" . >/dev/null
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o .dev/e2e/deploy.test ./internal/deploy

repo=$(pwd -W 2>/dev/null || pwd)
status=0
MSYS_NO_PATHCONV=1 docker run --rm \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v /tmp/lhtest:/tmp/lhtest \
	-v "$repo/.dev/e2e:/e2e" \
	-e LIGHTHOUSE_E2E_SELF_UPDATE="$base" \
	--entrypoint /e2e/deploy.test \
	"$base" -test.run TestRealSelfUpdate -test.v || status=$?

docker rmi -f "$base" >/dev/null 2>&1 || true
rm -rf .dev/e2e
exit $status
