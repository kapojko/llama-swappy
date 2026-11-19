#!/bin/bash

# Paths
MODEL_PATH="$HOME/models/ISTA-DASLab--Qwen3.8-27B-GSQ-RCO-GGUF/Qwen3.8-27B-GSQ-RCO-IQ3_XXS-mtp.gguf"
SERVER_BIN="$HOME/llama.cpp/build/bin/llama-server"

# Port: first positional argument (like the -Port parameter of the PowerShell
# scripts), else the PORT environment variable, else the default.
PORT="${1:-${PORT:-12380}}"

# Check if model exists
if [ ! -f "$MODEL_PATH" ]; then
    echo "Error: Model not found at $MODEL_PATH"
    exit 1
fi

echo "Starting llama.cpp HIP server..."

# Server run arguments
SERVER_ARGS=(
    -m "$MODEL_PATH"
    --no-mmproj

    --host 0.0.0.0
    --port "$PORT"

    --n-gpu-layers all
    -kvo
    --fit off
    -fa on

    #-c 98304
    -c 114688
    #-c 122880
    #-c 131072
    #-ctk q8_0 -ctv q8_0
    -ctk q5_1 -ctv q5_1
    #-ctk q4_0 -ctv q4_0
    -np 1
    #-b 1024 -ub 512
    -b 2048 -ub 512
    #-b 4096 -ub 1024

    --spec-type draft-mtp
    --spec-draft-n-max 3
    #--spec-draft-p-min 0.65
    --n-gpu-layers-draft all
    #--spec-draft-type-k q4_0 --spec-draft-type-v q4_0

    --reasoning on
    --reasoning-preserve
    --jinja
    --chat-template-kwargs {\"reasoning_effort\":\"medium\"}

    --temp 1.0
    --top-p 0.95
    --top-k 20
    --min-p 0.0
    --presence-penalty 0.0
    --repeat-penalty 1.0

    --threads $(nproc)
    --cache-ram 16384

    #-lv 4
)

# Run the server. exec matters: it replaces this shell with the server, so the
# server itself is llama-swappy's direct child and its signals reach it directly
# instead of a wrapper that dies first (see README, "Run scripts on Unix").
exec "$SERVER_BIN" "${SERVER_ARGS[@]}"