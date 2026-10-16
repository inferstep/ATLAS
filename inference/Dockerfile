# nvidia/cuda:12.9.0-devel-rockylinux9 — digest-pinned for reproducible builds (Dependabot maintains)
FROM docker.io/nvidia/cuda:12.9.2-devel-rockylinux9@sha256:7edd399cf796b6603a33ca5b3946dc8695c5d14f158abe4fea6dfb12012035b2 AS builder
RUN dnf install -y git cmake gcc-c++ && dnf clean all
# Pin llama.cpp to a known-good SHA so the PC-202 patch applies cleanly.
# Keep aligned with Dockerfile.v31 / Dockerfile.rocm / Dockerfile.vulkan.
ARG LLAMA_CPP_REV=2e97c5f96f9fe2bb26f794a348e05d7a1c74baa1
RUN git init /llama.cpp && \
    cd /llama.cpp && \
    git remote add origin https://github.com/ggml-org/llama.cpp && \
    git fetch --depth 1 origin ${LLAMA_CPP_REV} && \
    git checkout FETCH_HEAD && \
    # ATLAS patch: prevent --embeddings from poisoning the draft model context.
    # Without this, embedding=true propagates to the draft via params_dft = params_base,
    # causing output_all=true and n_ubatch mismatch → 0% spec decode acceptance.
    # The sed below IS the patch; CI verifies the target line still exists
    # at the pinned rev (test.yml llama-patches-apply).
    sed -i '/auto params_dft = params_base;/a\        params_dft.embedding = false;  // ATLAS: draft never needs embeddings' \
        tools/server/server-context.cpp && \
    # GGML_NATIVE off: the CPU part is built for a named processor level (SSE4.2,
    # AVX, AVX2, BMI2, FMA, F16C), not for the processor of the build machine.
    cmake -B build -DGGML_CUDA=ON -DGGML_NATIVE=OFF -DBUILD_SHARED_LIBS=OFF -DCMAKE_CUDA_ARCHITECTURES="120;121" && \
    cmake --build build --config Release -j$(nproc)

# nvidia/cuda:12.9.0-runtime-rockylinux9 — digest-pinned for reproducible builds (Dependabot maintains)
FROM docker.io/nvidia/cuda:12.9.2-runtime-rockylinux9@sha256:663416e2b4c199399ead053e3250bdbab3f124db696a7d01f26e215d18ecbd44
COPY --from=builder /llama.cpp/build/bin/llama-server /usr/local/bin/
COPY --from=builder /llama.cpp/build/bin/llama-cli /usr/local/bin/
RUN mkdir -p /models
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# The entrypoint's default listen port (PORT env) is 8080.
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
