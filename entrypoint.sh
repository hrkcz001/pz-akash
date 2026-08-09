#!/bin/bash

echo "=== Setting up Directories ==="
mkdir -p /home/steam/Zomboid/Server /home/steam/Zomboid/Saves /home/steam/Zomboid/db

echo "=== Copying Configs ==="
cp /home/steam/vsrania.ini /home/steam/Zomboid/Server/vsrania.ini
cp /home/steam/vsrania_SandboxVars.lua /home/steam/Zomboid/Server/vsrania_SandboxVars.lua

# Функция мягкой остановки при завершении работы контейнера
graceful_shutdown() {
    echo "=== Termination signal received! Shutting down PZ server gracefully... ==="
    # Посылаем сигнал завершения самому серверу PZ
    if kill -0 $PZ_PID 2>/dev/null; then
        kill -TERM $PZ_PID
        echo "Waiting for server to save local files and exit..."
        wait $PZ_PID
    fi
    exit 0
}

# Перехват сигналов (чтобы мир не повредился при рестарте/остановке контейнера)
trap graceful_shutdown SIGTERM SIGINT

echo "=== Starting Project Zomboid Dedicated Server ==="
PZ_PATH=$(find /home/steam/pz-server / -name "start-server.sh" -o -name "StartServer64.sh" 2>/dev/null | head -n 1)
if [ -z "$PZ_PATH" ]; then
    echo "ERROR: Server launch script not found!"
    exit 1
fi
PZ_DIR=$(dirname "$PZ_PATH")
chmod +x "$PZ_PATH" "$PZ_DIR"/ProjectZomboid64 "$PZ_DIR"/jre64/bin/java 2>/dev/null || true

cd "$PZ_DIR"
# Запуск с флагом -nosteam
"$PZ_PATH" -nosteam -servername vsrania -adminpassword "Qwerty01234**" -cachedir=/home/steam/Zomboid -Xmx8192m -Xms8192m &

PZ_PID=$!
wait $PZ_PID
EXIT_CODE=$?
echo "=== Project Zomboid Server exited with code $EXIT_CODE ==="

# Оставляем паузу на случай падения, чтобы успеть вытащить логи
if [ $EXIT_CODE -ne 0 ]; then
    echo "ERROR: Server crashed unexpectedly! Sleeping for 30 minutes to preserve logs for debugging..."
    sleep 1800
fi