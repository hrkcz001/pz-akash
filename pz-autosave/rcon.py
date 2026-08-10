import socket
import struct
import sys

def rcon_command(host, port, password, command):
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(60) # 60 seconds timeout for a large save to complete
        s.connect((host, port))
        
        # Auth
        packet_id = 1
        packet_type = 3 # SERVERDATA_AUTH
        body = password.encode('utf-8') + b'\x00\x00'
        size = 10 + len(body)
        s.send(struct.pack('<iii', size, packet_id, packet_type) + body)
        
        # First response is a generic response
        resp_size, resp_id, resp_type = struct.unpack('<iii', s.recv(12))
        s.recv(resp_size - 8)
        
        # Second response is the actual Auth response
        resp_size, resp_id, resp_type = struct.unpack('<iii', s.recv(12))
        s.recv(resp_size - 8)
        
        if resp_id == -1:
            print("RCON Auth Failed")
            return False
            
        # Command
        packet_id = 2
        packet_type = 2 # SERVERDATA_EXECCOMMAND
        body = command.encode('utf-8') + b'\x00\x00'
        size = 10 + len(body)
        s.send(struct.pack('<iii', size, packet_id, packet_type) + body)
        
        # Read Command response - this blocks until the command completes!
        resp_size, resp_id, resp_type = struct.unpack('<iii', s.recv(12))
        response_body = s.recv(resp_size - 8)
        
        print("RCON Response:", response_body.decode('utf-8', errors='ignore').strip('\x00'))
        
        s.close()
        return True
    except Exception as e:
        print(f"RCON Error: {e}")
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
