#!/bin/bash

# Paths
#MODEL_PATH="$HOME/models/Ornith-1.5-9B-GGUF/Ornith-1.5-9B-Q4_K_M.gguf"
MODEL_PATH="$HOME/models/Ornith-1.5-9B-GGUF/Ornith-1.5-9B-Q8_0.gguf"
SERVER_BIN="$HOME/llama.cpp/build/bin/llama-server"

# Check if model exists
if [ ! -f "$MODEL_PATH" ]; then
    echo "Error: Model not found at $MODEL_PATH"
    exit 1
fi

echo "Starting llama.cpp HIP server..."

# NOTE:
# Recommended sampling parameters:
#   For general tasks: temperature=1.0, top_p=0.95, top_k=20, min_p=0.0, presence_penalty=1.5, repetition_penalty=1.0
#   For precise coding tasks: temperature=0.6, top_p=0.95, top_k=20, min_p=0.0, presence_penalty=0.0, repetition_penalty=1.0

# Server run arguments
SERVER_ARGS=(
    -m "$MODEL_PATH"
    --host 0.0.0.0
    --port ${PORT:-12380}

    --n-gpu-layers all
    --kv-offload
    --fit off
    -fa on

    #-c 98304
    #-c 114688
    -c 147456
    #-c 155648
    #-c 163840
    #-c 196608
    -ctk bf16 -ctv bf16
    #-ctk f16 -ctv f16
    #-ctk q8_0 -ctv q8_0
    -np 1

    # -b 2048 -ub 512

    --spec-type draft-mtp
    --spec-draft-n-max 3
    #--spec-draft-p-min 0.65
    --n-gpu-layers-draft all

    --reasoning on
    --reasoning-preserve

    --temp 0.6
    --top-p 0.95
    --top-k 20
    --min-p 0.0
    --presence-penalty 0.0
    --repeat-penalty 1.0

    --threads $(nproc)
    --cache-ram 16384
)

# Run the server
$SERVER_BIN "${SERVER_ARGS[@]}"
