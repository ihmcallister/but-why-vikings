default: serve

serve:
    cd site && hugo server -D

sync-episodes:
    cd site && go run ./cmd/episode-sync

build:
    cd site && go run ./cmd/episode-sync && hugo
