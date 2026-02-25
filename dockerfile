FROM debian:bullseye-slim

# Setup 32-bit architecture and install tools
RUN dpkg --add-architecture i386 && \
    sed -i 's/main/main contrib non-free/g' /etc/apt/sources.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    wine wine32 libwine:i386 \
    xvfb xauth xdotool imagemagick scrot cabextract tini procps curl wget ca-certificates \
    # FIX: The URL must be the full path to the raw script file
    && curl -fL https://raw.githubusercontent.com/Winetricks/winetricks/master/src/winetricks \
       -o /usr/local/bin/winetricks && \
    chmod +x /usr/local/bin/winetricks && \
    # This check will now pass because it's a real shell script
    head -n 1 /usr/local/bin/winetricks | grep -q "#!" && \
    apt-get clean && rm -rf /var/lib/apt/lists/*


# Environment variables for headless Wine
ENV WINEARCH=win32
ENV WINEPREFIX=/home/aomuser/.wine
ENV DISPLAY=:99
ENV DEBIAN_FRONTEND=noninteractive

RUN useradd -m aomuser
USER aomuser
WORKDIR /home/aomuser

# Install DirectPlay (Needs xvfb to trick it into thinking a screen exists)
RUN xvfb-run -a winetricks -q directplay

# Copy your pre-installed game files
COPY --chown=aomuser:aomuser ./aom_files /home/aomuser/aom

# Change the working directory to the ACTUAL game folder
WORKDIR /home/aomuser/aom

# Start the virtual screen and the game
ENTRYPOINT ["tini", "--"]
# We use -n 99 to match your ENV DISPLAY=:99 explicitly
# We put -ac inside the -s string so it's passed directly to the Xvfb server
CMD ["xvfb-run", "-n", "99", "-s", "-screen 0 640x480x16 -ac", "wine", "/home/aomuser/aom/aomxnocd1.exe", "xres=640", "yres=480", "NoIntroCinematics", "NoSound"]

