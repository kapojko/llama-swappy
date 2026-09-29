#!/bin/bash

# Paths
MODEL_PATH="$HOME/models/NANI-Nithin--K2-Horizon-7B-GGUF/K2-Horizon-7B-Q8_0.gguf"
SERVER_BIN="$HOME/llama.cpp-mbzuai-ifm/build/bin/llama-server"

# Check if model exists
if [ ! -f "$MODEL_PATH" ]; then
    echo "Error: Model not found at $MODEL_PATH"
    exit 1
fi

echo "Starting llama.cpp HIP server..."

# NOTE:
# Recommended sampling parameters:
# Best Practices
# Reasoning effort: always high. All reported results use high reasoning effort. Pass {"chat_template_kwargs": {"reasoning_effort": "high"}} on every request; medium and low trade accuracy for speed and are not recommended for evaluation.
# Sampling parameters. temperature=1.0, top_p=0.95.
# Output length. Allow at least 32,768 output tokens so reasoning is never cut off. Truncated reasoning is a failed response, not a shorter one.

# Server run arguments
SERVER_ARGS=(
    -m "$MODEL_PATH"
    --host 0.0.0.0
    --port ${PORT:-12380}

    --n-gpu-layers all
    -fa on
    --fit off

    #-c 98304
    #-c 114688
    -c 131072
    #-ctk q8_0 -ctv q8_0
    -ctk q4_0 -ctv q4_0
    -np 1

    # -b 2048 -ub 512

    --reasoning on
    --reasoning-preserve

    --chat-template-kwargs {\"reasoning_effort\":\"high\"}
    --temp 1.0
    --top-p 0.95
    --top-k 20
    --min-p 0.0
    --presence-penalty 0.0
    --repeat-penalty 1.0
    --threads $(nproc)
)

# Run the server
$SERVER_BIN "${SERVER_ARGS[@]}"
