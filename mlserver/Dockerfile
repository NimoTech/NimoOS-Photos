# syntax=docker/dockerfile:1
# Single image covering Intel GPU (OpenVINO EP) and pure CPU.
# FP32 everywhere -- numerical parity with immich-ml is a hard requirement.
#
# Base OS deviation from the original plan: python:3.11-slim-bookworm
# (Debian 12, glibc 2.36) cannot satisfy the Intel GPU userspace stack
# below -- intel-opencl-icd 26.18.x declares `libc6 (>= 2.38)`. This host
# runs Debian 13 (trixie, glibc 2.41) and its Arc Pro B60 + iGPU golden
# runs were validated against that exact driver stack, so the image uses
# python:3.11-slim-trixie to match the host precisely. Same Python 3.11
# interpreter the venv-based golden gates ran on -- only the base OS
# changed.
#
# insightface has no prebuilt wheel for cp311/manylinux, so it is compiled
# in a throwaway builder stage (build-essential + cython) and only the
# resulting wheels are copied into the final image -- no compiler toolchain
# ships in the runtime layer.

############################
# Stage 1: wheel builder
############################
FROM python:3.11-slim-trixie AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /wheels
COPY requirements.txt .
# --no-deps: requirements.txt is an authoritative, already-flattened lock
# of every runtime dependency (verified against the golden-tested host
# venv). Without --no-deps, pip additionally resolves rapidocr's own
# declared `opencv_python` dependency and pulls in real (non-headless,
# X11-linked) opencv-python alongside the pinned opencv-python-headless --
# two packages that both ship a `cv2` module and silently clobber each
# other's files in site-packages, breaking the cv2 import (libxcb.so.1
# missing) at runtime. --no-deps keeps exactly the pinned closed set.
RUN pip wheel --no-cache-dir --no-deps -r requirements.txt -w /wheels

############################
# Stage 2: runtime image
############################
FROM python:3.11-slim-trixie

# --- Intel GPU userspace: OpenCL (compute-runtime) + Level Zero loader ---
# None of this is in Debian's own archive (confirmed on the host: these
# packages show up as "local"/dpkg-installed, not from any configured apt
# repo) -- pull the *exact* upstream GitHub release builds already
# validated on this host's Arc Pro B60 + iGPU so the container shares one
# driver stack with the host:
#   - intel/compute-runtime  26.18.38308.1  -> intel-opencl-icd, libze-intel-gpu1, libigdgmm12
#   - intel/intel-graphics-compiler v2.34.4 -> intel-igc-core-2, intel-igc-opencl-2
#   - oneapi-src/level-zero  v1.28.6        -> libze1 (loader; +u24.04 build, closest glibc match to trixie)
# ocl-icd-libopencl1 (the ICD loader itself) *is* a normal Debian package.
ARG CR_TAG=26.18.38308.1
ARG CR_VER=26.18.38308.1-0
ARG IGC_TAG=v2.34.4
ARG IGC_VER=2.34.4+21428
ARG LZ_TAG=v1.28.6
ARG LZ_VER=1.28.6+u24.04
ARG GMM_VER=22.10.0

RUN apt-get update && apt-get install -y --no-install-recommends \
        tini libgomp1 libglib2.0-0 ocl-icd-libopencl1 ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && tmp="$(mktemp -d)" && cd "$tmp" \
    && CURL="curl -fsSL --retry 5 --retry-all-errors --max-time 60 -O" \
    && $CURL "https://github.com/intel/intel-graphics-compiler/releases/download/${IGC_TAG}/intel-igc-core-2_${IGC_VER}_amd64.deb" \
    && $CURL "https://github.com/intel/intel-graphics-compiler/releases/download/${IGC_TAG}/intel-igc-opencl-2_${IGC_VER}_amd64.deb" \
    && $CURL "https://github.com/intel/compute-runtime/releases/download/${CR_TAG}/libigdgmm12_${GMM_VER}_amd64.deb" \
    && $CURL "https://github.com/intel/compute-runtime/releases/download/${CR_TAG}/intel-opencl-icd_${CR_VER}_amd64.deb" \
    && $CURL "https://github.com/intel/compute-runtime/releases/download/${CR_TAG}/libze-intel-gpu1_${CR_VER}_amd64.deb" \
    && $CURL "https://github.com/oneapi-src/level-zero/releases/download/${LZ_TAG}/libze1_${LZ_VER}_amd64.deb" \
    && apt-get install -y ./*.deb \
    && cd / && rm -rf "$tmp" \
    && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
# Bind-mount (not COPY) the builder's wheelhouse: a COPY would bake ~320MB
# of wheel files into this layer, and deleting them in the *next* layer
# doesn't shrink the image (a whiteout hides them, but every prior layer's
# bytes still ship) -- the mount never becomes part of any layer at all.
RUN --mount=type=bind,from=builder,source=/wheels,target=/wheels \
    pip install --no-cache-dir --no-deps --no-index --find-links=/wheels -r requirements.txt

COPY server/ server/

ENV MLSERVER_CACHE=/cache MLSERVER_TTL=300 MLSERVER_DEVICE=auto
# albumentations does a PyPI version-check network call on import by
# default; disable it so cold starts don't depend on outbound internet.
ENV NO_ALBUMENTATIONS_UPDATE=1
EXPOSE 3003
ENTRYPOINT ["tini", "--"]
CMD ["python", "-m", "uvicorn", "server.main:app", "--host", "0.0.0.0", "--port", "3003"]
