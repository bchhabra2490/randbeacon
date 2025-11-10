# RandBeacon

A hybrid random beacon service that aggregates entropy from multiple cryptocurrency exchange sources to generate verifiable, unpredictable random numbers.

## Overview

RandBeacon is a distributed random number generation service that combines public entropy sources with a private secret to produce cryptographically secure random values. The system collects real-time trade data from multiple cryptocurrency exchanges (Binance, Coinbase, Kraken) and processes it through a hybrid entropy model to generate beacon entries approximately every second.

## Demo Video

Watch a quick demo of RandBeacon in action:

[![RandBeacon Demo](https://img.youtube.com/vi/di4FXnXcDeE/maxresdefault.jpg)](https://www.youtube.com/watch?v=di4FXnXcDeE)

[Watch on YouTube](https://www.youtube.com/watch?v=di4FXnXcDeE)

## How It Works

### Entropy Collection

The system collects entropy from multiple sources:

- **Binance**: Real-time trade data for BTC/USDT and ETH/USDT pairs
- **Coinbase**: Real-time trade matches for BTC-USD and ETH-USD pairs
- **Kraken**: Real-time trade data for XBT/USD and ETH/USD pairs

Each trade event is captured with its source identifier, timestamp, and raw message bytes, then aggregated into a snapshot buffer.

### Beacon Generation

Every second (configurable via `TickInterval`), the system:

1. **Aggregates Entropy**: Collects all entropy events received since the last beacon into a snapshot
2. **Generates Public Entropy**: Creates a publicly verifiable hash from:
   - Current snapshot (aggregated trade events)
   - Previous public entropy (for chaining)
   - Current timestamp
3. **Generates Final Random**: Combines public entropy with a private secret using HMAC to produce the final unpredictable random value

## Public Entropy vs Final Random

### Public Entropy (`public_entropy_hex`)

**Public Entropy** is a **publicly verifiable** random value that anyone can independently verify. It is computed as:

```
Public Entropy = SHA3-512(snapshot || previous_public_entropy || timestamp)
```

**Characteristics:**
- ✅ **Verifiable**: Anyone with the snapshot, previous public entropy, and timestamp can verify the result
- ✅ **Transparent**: All inputs are public and can be audited
- ✅ **Chainable**: Each entry links to the previous one, creating an immutable chain
- ⚠️ **Potentially Predictable**: If someone knows all the inputs, they could theoretically predict the output

**Use Cases:**
- Public lotteries where transparency is required
- Verifiable random number generation for public protocols
- Auditable randomness for public systems

### Final Random (`final_random_hex`)

**Final Random** is an **unpredictable** random value that combines public entropy with a private secret. It is computed as:

```
Private Component = HMAC-SHA256(public_entropy, local_secret)
Final Random = SHA3-512(public_entropy || private_component)
```

**Characteristics:**
- ✅ **Unpredictable**: Even with full knowledge of all public inputs, the output cannot be predicted without the secret
- ✅ **Secure**: The private secret (`local_secret`) is never exposed
- ✅ **Hybrid Model**: Combines public verifiability with private unpredictability
- ⚠️ **Not Fully Verifiable**: The private component cannot be verified by third parties

**Use Cases:**
- Cryptographically secure random number generation
- Applications requiring unpredictable randomness
- Systems where security is more important than public verifiability

### Key Differences

| Aspect | Public Entropy | Final Random |
|--------|---------------|--------------|
| **Verifiability** | Fully verifiable | Not verifiable (secret component) |
| **Predictability** | Potentially predictable | Unpredictable |
| **Transparency** | Fully transparent | Opaque (secret component) |
| **Security** | Lower (public inputs) | Higher (includes secret) |
| **Use Case** | Public protocols, lotteries | Secure applications |

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  Binance    │     │  Coinbase   │     │   Kraken    │
│  WebSocket  │     │  WebSocket  │     │  WebSocket  │
└──────┬──────┘     └──────┬──────┘     └──────┬──────┘
       │                   │                    │
       └───────────────────┼────────────────────┘
                            │
                    ┌───────▼────────┐
                    │  Entropy Channel│
                    │   (Buffered)    │
                    └───────┬─────────┘
                            │
                    ┌───────▼────────┐
                    │   Aggregator   │
                    │  (Every 1 sec) │
                    └───────┬────────┘
                            │
            ┌───────────────┼───────────────┐
            │                               │
    ┌───────▼────────┐           ┌─────────▼────────┐
    │ Public Entropy │           │  Final Random    │
    │  (Verifiable)  │           │ (Unpredictable)  │
    └────────────────┘           └──────────────────┘
```

## Features

- **Multi-Source Entropy**: Aggregates data from 3 major cryptocurrency exchanges
- **Hybrid Model**: Combines public verifiability with private unpredictability
- **Real-Time Updates**: WebSocket stream for live beacon entries
- **REST API**: Full API for querying latest entries, history, and snapshots
- **HKDF Derivation**: Derive application-specific random values from beacon entries
- **Snapshot Storage**: Stores raw entropy snapshots for verification (rolling buffer)

## API Endpoints

- `GET /random/latest` - Get the latest beacon entry
- `GET /random/history?n=10` - Get recent beacon entries
- `GET /random/snapshot/:index` - Get raw snapshot data for verification
- `POST /random/derive` - Derive random bytes using HKDF
- `WS /stream` - WebSocket stream for real-time updates

See [API Documentation](http://localhost:8080/docs) for complete details.

## Installation

1. Clone the repository:
```bash
git clone <repository-url>
cd randbeacon
```

2. Install dependencies:
```bash
go mod download
```

3. Run the service:
```bash
go run main.go
```

The service will start on `http://localhost:8080`

## Configuration

Key constants in `main.go`:

- `TickInterval`: Time between beacon generations (default: 1 second)
- `EntropyChanSize`: Buffer size for entropy events (default: 32768)
- `HistorySize`: Maximum history entries stored (default: 4096)
- `MaxSnapshotStore`: Maximum snapshots stored (default: 8192)
- `PublicEntropyLen`: Length of public entropy in bytes (default: 64)
- `FinalRandonLen`: Length of final random in bytes (default: 64)

## Security Considerations

- **Local Secret**: The `local_secret.bin` file contains a 32-byte secret used for HMAC computation. This file should be kept secure and never exposed.
- **Secret Generation**: On first run, the secret is generated from `/dev/urandom` or system entropy.
- **Snapshot Privacy**: Snapshots contain raw trade data and should be considered sensitive if privacy is a concern.

