import socket
import struct
import sys

import traceback

def rcon_command(host, port, password, command):
    print(f"RCON: Connecting to {host}:{port}...")
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(60) # 60 seconds timeout for a large save to complete
        s.connect((host, port))
        print("RCON: Connected successfully. Sending Auth...")
        
        # Auth
        packet_id = 1
        packet_type = 3 # SERVERDATA_AUTH
        body = password.encode('utf-8') + b'\x00\x00'
        size = 10 + len(body)
        s.send(struct.pack('<iii', size, packet_id, packet_type) + body)
        
        print("RCON: Auth sent. Waiting for response...")
        # Read Auth response
        resp_data = s.recv(12)
        if not resp_data:
            print("RCON Error: Connection closed by server while waiting for Auth response")
            return False
            
        resp_size, resp_id, resp_type = struct.unpack('<iii', resp_data)
        print(f"RCON: Received packet 1 -> size={resp_size}, id={resp_id}, type={resp_type}")
        s.recv(resp_size - 8)
        
        # Some servers send a dummy response (type 0) before the auth response (type 2)
        if resp_type != 2:
            print("RCON: Packet 1 was not Auth Response. Waiting for packet 2...")
            resp_data = s.recv(12)
            if not resp_data:
                print("RCON Error: Connection closed while waiting for packet 2")
                return False
            resp_size, resp_id, resp_type = struct.unpack('<iii', resp_data)
            print(f"RCON: Received packet 2 -> size={resp_size}, id={resp_id}, type={resp_type}")
            s.recv(resp_size - 8)
        
        if resp_id == -1:
            print("RCON Auth Failed")
            return False
            
        print("RCON: Auth successful. Sending command...")
        # Command
        packet_id = 2
        packet_type = 2 # SERVERDATA_EXECCOMMAND
        body = command.encode('utf-8') + b'\x00\x00'
        size = 10 + len(body)
        s.send(struct.pack('<iii', size, packet_id, packet_type) + body)
        
        print("RCON: Command sent. Waiting for response...")
        # Read Command response - this blocks until the command completes!
        resp_data = s.recv(12)
        if not resp_data:
             print("RCON Error: Connection closed while waiting for command response")
             return False
        resp_size, resp_id, resp_type = struct.unpack('<iii', resp_data)
        response_body = s.recv(resp_size - 8)
        
        print("RCON Response:", response_body.decode('utf-8', errors='ignore').strip('\x00'))
        
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
