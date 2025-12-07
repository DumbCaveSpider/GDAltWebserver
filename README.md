# Alternative Geometry Dash Account Backup Server

The backend server for [GD Account Backup](https://github.com/DumbCaveSpider/GDAccountBackup).

## Usage (Server)
You need the following requirements to run the server:
- [Node.js](https://nodejs.org/) (v23 or higher)
- [Go Language](https://go.dev/) 1.25.1 or higher
- A database (e.g., MySQL, PostgreSQL)

1. Clone the repository:

```bash
git clone https://github.com/DumbCaveSpider/GDAltWebserver.git
cd GDAltWebserver
```

2. Install dependencies:

```bash
npm install
```
3. Configure the server by creating a `.env` file in the root directory with the following content:

```env
DB_USER=<your_database_user>
DB_PASS=<your_database_password>
DB_HOST=<your_database_host>
DB_PORT=<your_database_port>
DB_NAME=<your_database_name>
ARGON_BASE_URL=https://argon.globed.dev/v1/validation/check
MAX_DATA_SIZE_BYTES=33554432
LOG_LEVEL=1 # 0=Error, 1=Info, 2=Debug
PORT=3001
```

4. Build and run the server:

```bash
npm run prod
```

The server will start on `http://localhost:3001` by default.

## Usage (Client)
Go to the mod settings of Account Backup in Geometry Dash and set the Backup Server URL to your server's address (e.g., `http://localhost:3001`)

<img width="765" height="72" alt="image" src="https://github.com/user-attachments/assets/345ea290-fabc-40ff-a64a-fd0babf763a6" />
