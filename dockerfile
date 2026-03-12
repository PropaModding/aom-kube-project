FROM debian:bullseye-slim

# 1. System Setup (Runs as root)
RUN dpkg --add-architecture i386 && \
    # FIX: Use the archive URL with the correct /debian/ path
    echo "deb [check-valid-until=no] http://archive.debian.org/debian bullseye-backports main" > /etc/apt/sources.list.d/backports.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    wine wine32 libwine libgl1-mesa-dri:i386 libgl1-mesa-glx:i386 mesa-utils:i386 \
    xvfb xauth xdotool imagemagick scrot x11-utils cabextract tini procps curl wget \
    xfonts-base xfonts-100dpi xfonts-75dpi ca-certificates \
    # FIX: Use the complete RAW URL for the Winetricks script
    && curl -fL https://raw.githubusercontent.com/Winetricks/winetricks/master/src/winetricks -o /usr/local/bin/winetricks \
    && chmod +x /usr/local/bin/winetricks \
    # Verification: Ensures the file is a shell script
    && head -n 1 /usr/local/bin/winetricks | grep -q "#!" \
    && mkdir -p /tmp/.X11-unix && chmod 1777 /tmp/.X11-unix \
    && apt-get clean && rm -rf /var/lib/apt/lists/*


# 2. Environment & User Setup
ENV WINEARCH=win32
ENV WINEPREFIX=/home/aomuser/.wine
ENV DISPLAY=:99
ENV WINEDEBUG=-all

RUN useradd -m aomuser && \
    groupadd -f video && groupadd -f render && \
    usermod -aG video,render aomuser

USER aomuser
WORKDIR /home/aomuser

# 3. CONSOLIDATED Wine Setup (Baking the "Titans" settings)
# Using ; instead of && to ensure non-critical Wine warnings don't break the build
RUN Xvfb :99 -screen 0 800x600x16 -ac -nolisten unix & \
    sleep 5 ; \
    export WINEDLLOVERRIDES="mscoree,mshtml=" ; \
    wineboot --init ; wineserver -w ; \
    # FIX: EULA & PID for THE TITANS EXPANSION specifically
    wine reg add "HKEY_CURRENT_USER\Software\Ensemble Studios\Age of Mythology Expansion\1.0" /v "EULA" /t REG_DWORD /d "1" /f ; \
    wine reg add "HKEY_CURRENT_USER\Software\Ensemble Studios\Age of Mythology Expansion\1.0" /v "PID" /t REG_SZ /d "11111-11111-11111-11111-11111" /f ; \
    # BAKE: Base game EULA too just in case
    wine reg add "HKEY_CURRENT_USER\Software\Ensemble Studios\Age of Mythology\1.0" /v "EULA" /t REG_DWORD /d "1" /f ; \
    # FIX: Video Card Detection (Fake Video RAM to 256MB)
    wine reg add "HKEY_CURRENT_USER\Software\Wine\Direct3D" /v "VideoMemorySize" /t REG_SZ /d "256" /f ; \
    # BAKE: Resolution settings in Registry
    wine reg add "HKEY_CURRENT_USER\Software\Ensemble Studios\Age of Mythology Expansion\1.0" /v "ScreenWidth" /t REG_DWORD /d "800" /f ; \
    wine reg add "HKEY_CURRENT_USER\Software\Ensemble Studios\Age of Mythology Expansion\1.0" /v "ScreenHeight" /t REG_DWORD /d "600" /f ; \
    # BAKE: Stability settings (Disable Sound & SHM)
    wine reg add "HKEY_CURRENT_USER\Software\Wine\Drivers" /v "Audio" /t REG_SZ /d "" /f ; \
    wine reg add "HKEY_CURRENT_USER\Software\Wine\X11 Driver" /v "UseShm" /t REG_SZ /d "N" /f ; \
    # Install Components
    /usr/local/bin/winetricks -q directplay ; wineserver -w ; \
    /usr/local/bin/winetricks -q msxml4 mfc42 d3dx9 ; wineserver -w ; \
    # Final flush
    sleep 5 ; wineserver -k || true

# 4. Finalise Files & Configuration
COPY --chown=aomuser:aomuser ./aom_files /home/aomuser/aom
WORKDIR /home/aomuser/aom

# FIX: Create engine-level user.cfg to force 800x600 and skip Movieplayer.exe
# We use 'overrideResolution' to force the engine to respect our xres/yres
RUN mkdir -p /home/aomuser/aom/startup && \
    echo "xres=800" > /home/aomuser/aom/startup/user.cfg && \
    echo "yres=600" >> /home/aomuser/aom/startup/user.cfg && \
    echo "noIntroCinematics" >> /home/aomuser/aom/startup/user.cfg && \
    echo "lowDetail" >> /home/aomuser/aom/startup/user.cfg && \
    echo "window" >> /home/aomuser/aom/startup/user.cfg && \
    echo "noSound" >> /home/aomuser/aom/startup/user.cfg && \
    echo "overrideResolution" >> /home/aomuser/aom/startup/user.cfg

ENTRYPOINT ["tini", "--"]
# Starting a shell so you can manually run or automate from here
CMD ["/bin/bash"]

