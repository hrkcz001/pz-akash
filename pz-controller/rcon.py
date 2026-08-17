import socket
import struct
import sys
import traceback


def recvall(sock, n):
    """Read exactly n bytes from sock, looping as needed (TCP may fragment)."""
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError(f"Connection closed after {len(buf)}/{n} bytes")
        buf += chunk
    return buf


def rcon_command(host, port, password, command):
    print(f"RCON: Connecting to {host}:{port}...")
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(60)  # 60 seconds timeout for a large save to complete
        s.connect((host, port))
        print("RCON: Connected successfully. Sending Auth...")

        # Auth packet: size(4) | id(4) | type(4) | body | NUL | NUL-pad
        # type 3 = SERVERDATA_AUTH
        packet_id = 1
        packet_type = 3
        body = password.encode("utf-8") + b"\x00\x00"
        size = 8 + len(body)  # id(4) + type(4) + body (includes both NULs)
        s.send(struct.pack("<iii", size, packet_id, packet_type) + body)

        print("RCON: Auth sent. Waiting for response...")

        # Read Auth response header (always exactly 12 bytes)
        resp_data = recvall(s, 12)
        resp_size, resp_id, resp_type = struct.unpack("<iii", resp_data)
        print(f"RCON: Received packet 1 -> size={resp_size}, id={resp_id}, type={resp_type}")
        recvall(s, resp_size - 8)  # drain body

        # Some servers send a dummy response (type 0) before the auth response (type 2)
        if resp_type != 2:
            print("RCON: Packet 1 was not Auth Response. Waiting for packet 2...")
            resp_data = recvall(s, 12)
            resp_size, resp_id, resp_type = struct.unpack("<iii", resp_data)
            print(f"RCON: Received packet 2 -> size={resp_size}, id={resp_id}, type={resp_type}")
            recvall(s, resp_size - 8)  # drain body

        if resp_id == -1:
            print("RCON Auth Failed")
            return False

        print("RCON: Auth successful. Sending command...")

        # Command packet: type 2 = SERVERDATA_EXECCOMMAND
        packet_id = 2
        packet_type = 2
        body = command.encode("utf-8") + b"\x00\x00"
        size = 8 + len(body)
        s.send(struct.pack("<iii", size, packet_id, packet_type) + body)

        print("RCON: Command sent. Waiting for response...")
        # Read command response header — blocks until the command completes
        resp_data = recvall(s, 12)
        resp_size, resp_id, resp_type = struct.unpack("<iii", resp_data)
        response_body = recvall(s, resp_size - 8)

        print("RCON Response:", response_body.decode("utf-8", errors="ignore").strip("\x00"))

        s.close()
        return True

    except socket.timeout:
        print("RCON Error: Socket timed out. Traceback:")
        traceback.print_exc()
        return False
    except Exception as e:
        print(f"RCON Error: {e}")
        traceback.print_exc()
        return False


if __name__ == "__main__":
    if len(sys.argv) != 5:
        print("Usage: rcon.py <ip> <port> <password> <command>")
        sys.exit(1)
    host = sys.argv[1]
    port = int(sys.argv[2])
    password = sys.argv[3]
    command = sys.argv[4]
    if rcon_command(host, port, password, command):
        sys.exit(0)
    sys.exit(1)
