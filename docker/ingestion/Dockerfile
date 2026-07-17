FROM python:3.11-slim-bookworm

# Install system deps for pycairo + runtime libcairo2
# Separate update/install to avoid dpkg race conditions in Kaniko
RUN apt-get update && \
    apt-get -y --no-install-recommends install \
        pkg-config gcc python3-dev \
        libcairo2-dev libcairo2 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Install Python deps
RUN pip install --no-cache-dir rmscene qdrant-client httpx pycairo

WORKDIR /app