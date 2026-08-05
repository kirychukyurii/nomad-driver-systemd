FROM debian:12

# Install necessary packages
RUN apt-get update && apt-get install -y \
    build-essential \
    git \
    curl \
    wget \
    ca-certificates \
    libsystemd-dev \
    pkg-config \
    systemd \
    && rm -rf /var/lib/apt/lists/*

# Create workspace directory
RUN mkdir -p /workspace
WORKDIR /workspace

# Set default shell to bash
SHELL ["/bin/bash", "-c"]

# Keep container running
CMD ["/bin/bash"]
