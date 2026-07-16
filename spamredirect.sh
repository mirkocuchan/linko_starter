#!/usr/bin/env bash

set -euo pipefail

iterations=$1

mkdir -p data
printf 'http://localhost:8899' > data/ABCDEF

for ((i = 1; i <= iterations; i++)); do
  curl -sS "http://localhost:8899/ABCDEF" > /dev/null
  if (( i % 100 == 0 )); then
    echo "Completed $i requests"
  fi
done