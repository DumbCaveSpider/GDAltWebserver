# Alternative Geometry Dash Account Backup Server

The backend server for [GD Account Backup](https://github.com/DumbCaveSpider/GDAccountBackup).

## Usage (Server)
### Docker (Recommended)
Before starting, ensure you have a MySQL database with `schema.sql` applied.

1. Install [Docker](https://www.docker.com/)

2. Clone the repository:

```bash
git clone https://github.com/DumbCaveSpider/GDAltWebserver.git
cd GDAltWebserver
```

3. Create a `.env` file in the root directory:

```env
DB_USER=<your_database_user>
DB_PASS=<your_database_password>
DB_HOST=<your_database_host>
DB_PORT=<your_database_port>
DB_NAME=<your_database_name>
ARGON_BASE_URL=https://argon.globed.dev/v1/validation/check
MAX_DATA_SIZE_BYTES=33554432
LOG_LEVEL=1 # 0=Debug, 1=Info, 2=Warn
PORT=3001
```

4. Build and run the container:
```
docker build -t gd-alt-webserver .
docker run -d \
  --name gd-alt-webserver \
  -p 3001:3001 \
  --env-file .env \
  gd-alt-webserver
```

The server will be available at http://localhost:3001.

### Node.js
You need the following requirements to run the server:
- [Node.js](https://nodejs.org/) (v23 or higher)
- [Go Language](https://go.dev/) 1.25.1 or higher
- A MySQL database

1. Clone the repository:

```bash
git clone https://github.com/DumbCaveSpider/GDAltWebserver.git
cd GDAltWebserver
```

2. Install dependencies:

```bash
npm install
```

3. Configure the server by creating a `.env` file in the root directory (as in the "Docker" guide)

4. Build and run the server:

```bash
npm run prod
```

The server will start on `http://localhost:3001` by default.

## Usage (Client)
Go to the mod settings of Account Backup in Geometry Dash and set the Backup Server URL to your server's address (e.g., `http://localhost:3001`)

<img width="765" height="72" alt="image" src="https://github.com/user-attachments/assets/345ea290-fabc-40ff-a64a-fd0babf763a6" />

## Server Authorization
To prevent anyone on the internet from using your server, you can set a token.
This token will need to be known to all users who wish to use your server.

First, add `AUTHORIZATION_TOKEN` to your `.env`:
```env
AUTHORIZATION_TOKEN=<your_token>
```

Next, go the mod settings in-game and set the Authorization Token to your custom token.