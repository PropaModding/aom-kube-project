FROM debian:bullseye-slim

# Setup 32-bit architecture for AoM (which is a 32-bit app)
RUN dpkg --add-architecture i386 && \
    sed -i 's/main/main contrib non-free/g' /etc/apt/sources.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    wine wine32 libwine:i386 \
    xvfb xauth xdotool cabextract tini procps curl wget ca-certificates \
    && curl -fL https://raw.githubusercontent.com/Winetricks/winetricks/master/src/winetricks \
       -o /usr/local/bin/winetricks && \
    chmod +x /usr/local/bin/winetricks && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Environment variables for headless Wine
ENV WINEARCH=win32
ENV WINEPREFIX=/home/aomuser/.wine
ENV DISPLAY=:99
ENV DEBIAN_FRONTEND=noninteractive

RUN useradd -m aomuser
USER aomuser
WORKDIR /home/aomuser

# Install DirectPlay (The networking heart of AoM)
# This step might take a minute as it downloads Microsoft components
RUN xvfb-run -a winetricks -q directplay

# Copy your pre-installed game files
COPY --chown=aomuser:aomuser ./aom_files /home/aomuser/aom

# Start the virtual screen and the game
ENTRYPOINT ["tini", "--"]
CMD ["xvfb-run", "-a", "-s", "-screen 0 640x480x16", "wine", "/home/aomuser/aom/aomx.exe", "xres=640", "yres=480", "NoIntroCinematics", "NoSound"]
