# The fake Lampac as a performance-run sidecar (deploy/rehearse/perf.sh
# --sidecar). It runs on another container's network namespace
# (docker run -d --network container:<id>), so it listens on that
# container's own loopback, 127.0.0.1:9118: the address both
# VAULT_LAMPAC_URL and ANTRESOL_ONLINE_LAMPAC_URL carry there. No port, no
# volume: everything it serves is in the image.
#
# Build context: a folder with
#   fakelampac   the static linux/amd64 program:
#                  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o <ctx>/fakelampac ./tools/fakelampac
#                (run from addons/online; mode 0755)
#   hls/         the synthetic video, made once where ffmpeg is:
#                  fakelampac -listen 127.0.0.1:0 -hls <ctx>/hls -seconds 44   (stop it once it prints its line)
#                (index.m3u8 and seg0.ts … ; files 0644, folder 0755)
# Tag it antresol-fakelampac:<sha>, the sha being the first 12 hex digits of
# the program's sha256 (the tag perf.sh accepts: antresol-fakelampac:<7-40 hex>).
FROM scratch
COPY fakelampac /fakelampac
COPY hls /hls
USER 65534:65534
ENTRYPOINT ["/fakelampac", "-listen", "127.0.0.1:9118", "-hls", "/hls"]
