# G.711 Smali Implementation Analysis

## Overview

This document analyzes the `G711.smali` implementation from the Android APK to understand how the custom G.711 codec works and how it differs from standard A-law and our current Go implementation.

## Key Findings

### 1. Table Structure

The Android implementation uses two tables:

- **`expansor_table`**: 256-entry decode table (already extracted to `tables/g711_extracted.json`)
- **`compresor_table`**: 4096-entry encode table (maps PCM >> 4 to G.711 codes)

### 2. Decode Method: `ExpandeG711(I)I`

**Input**: Unsigned byte (0-255)
- The Android code converts signed bytes to unsigned before calling: `if (byte < 0) byte += 0x100`

**Algorithm** (from `G711.smali` lines 24524-24555):
```java
// Step 1: Extract sign bit from inverted code
int sign_bit = ((code ^ 0xFF) << 8) & 0x8000;

// Step 2: Compute table index (XOR with 0x55)
int index = code ^ 0x55;

// Step 3: Lookup in expansor_table
int table_val = expansor_table[index];

// Step 4: Transform: multiply by 8, add 3, apply sign
int output = (table_val << 3) | 0x3 | sign_bit;
```

**Output**: Integer (range: 11 to 65531), then converted to `short` (16-bit signed PCM)

**Sign Handling**:
- Codes 0-127: sign bit is set (0x8000) → output is large positive, becomes negative when cast to short
- Codes 128-255: sign bit is clear (0x0000) → output is small positive

**Example**:
- Code `0x00`: index = `0x55`, table_val = 3408, output = `(3408 << 3) | 3 | 0x8000` = 60035 (as short: -5501)
- Code `0x80`: index = `0xD5`, table_val = 688, output = `(688 << 3) | 3 | 0x0000` = 5507 (as short: 5507)

### 3. Encode Method: `ComprimeG711(I)I`

**Input**: Integer (PCM sample, can be negative)

**Algorithm** (from `G711.smali` lines 24498-24522):
```java
// Step 1: Convert negative to unsigned 16-bit
if (pcm < 0) {
    pcm = pcm + 0x10000;
}

// Step 2: Shift right 4 bits
int index = pcm >> 4;

// Step 3: Lookup in compressor_table (4096 entries)
int code_raw = compressor_table[index];

// Step 4: XOR with 0x55
int code = code_raw ^ 0x55;
```

**Output**: Byte (0-255)

### 4. Audio Flow in Android App

**RX Path** (`VirtualPanel$2.smali`):
1. Receive UDP packet (0x10C bytes)
2. Extract audio payload at offset 0xC (256 bytes)
3. For each byte:
   - Convert signed byte to unsigned: `if (byte < 0) byte += 0x100`
   - Call `ExpandeG711(byte)` → returns int
   - Convert to short: `(short)result`
   - Store in short array (16-bit PCM)
4. Write to FIFO buffer for playback

**TX Path** (`VirtualPanel$1.smali`):
1. Read PCM samples from capture (short array)
2. For each sample:
   - Call `ComprimeG711(sample)` → returns int
   - Convert to byte: `(byte)result`
   - Store in byte array
3. Build UDP packet with audio payload at offset 0xC

## Comparison with Standard A-law

The Android custom implementation is **NOT** standard A-law:

1. **Table Index**: Uses `code ^ 0x55` instead of direct code
2. **Output Scaling**: Applies `(val << 3) | 3` transformation
3. **Sign Encoding**: Inverted (codes 0-127 = negative, codes 128-255 = positive)
4. **Output Range**: Much larger (±32k range) vs A-law's ±8k range

**Example Comparison**:
| Code | Android Custom | Standard A-law | Difference |
|------|----------------|----------------|------------|
| 0x00 | -5501         | -688          | ~8x larger |
| 0x55 | -5            | -1            | Similar    |
| 0x80 | 5507          | 688           | ~8x larger |
| 0xFF | 851           | 106           | ~8x larger |

## Comparison with Beltpack/Matrix Audio

Based on testing:
- **Beltpack/Matrix**: Uses standard G.711 A-law encoding
- **Android App**: Uses custom G.711 variant (as analyzed above)
- **Result**: The two are incompatible - beltpack audio decoded with Android's custom table sounds distorted/overdriven

## Current Go Implementation Issues

Our `g711Codec` in `custom` mode has several mismatches:

1. **Missing XOR 0x55**: We use direct table index instead of `code ^ 0x55`
2. **Missing Transform**: We don't apply `(val << 3) | 3` scaling
3. **Inverted Sign**: We treat `code & 0x80` as negative, but smali treats `code & 0x80 == 0` as negative
4. **Wrong Output Range**: We output ±4095 range, but smali outputs ±32k range

**Current Go decode**:
```go
v := c.decodeTbl[int(b)]
if (b & 0x80) != 0 {
    v = -v
}
out[i] = v
```

**Should be** (matching smali):
```go
sign_bit := ((b ^ 0xFF) << 8) & 0x8000
index := b ^ 0x55
table_val := c.decodeTbl[index]
output := (table_val << 3) | 0x3 | sign_bit
// Convert to signed 16-bit
if output >= 32768 {
    output = output - 65536
}
out[i] = int16(output)
```

## Recommendations

1. **For Beltpack/Matrix Audio**: Always use `--g711-mode alaw` (standard A-law)
2. **For Android App Compatibility**: Fix `custom` mode to match smali implementation exactly
3. **Default Mode**: Consider making `alaw` the default since beltpack/matrix is the primary use case

## Next Steps

1. Fix `g711Codec.decode()` in `custom` mode to match smali logic
2. Fix `g711Codec.encode()` in `custom` mode (requires extracting compressor table or building it from expansor)
3. Update `PROTOCOL.md` to document both variants clearly
4. Add tests comparing decoded output against smali reference

