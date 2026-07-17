FROM python:3.14-slim

# Install system deps for pycairo + runtime libcairo2
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        libcairo2-dev pkg-config gcc python3-dev \
        libcairo2 && \
    rm -rf /var/lib/apt/lists/*

# Install Python deps
RUN pip install --no-cache-dir rmscene qdrant-client httpx pycairo

WORKDIR /app