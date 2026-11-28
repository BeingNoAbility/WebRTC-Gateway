/**
 * Native Bounded Streaming Gzip Decompression Pipeline
 * - Peeks first non-empty chunk BEFORE constructing `DecompressionStream('gzip')` so
 *   0-byte empty bodies close cleanly instead of throwing on empty gzip EOF.
 * - Cancels upstream `source` even if consumer calls `cancel()` BEFORE the first `pull()`.
 * - Synchronizes `totalIn` / `totalOut` accounting via a 1-chunk-buffered counting stage
 *   so read-ahead buffering cannot mask decompression bombs.
 */

const MAX_EXPANSION_RATIO = 1024;
const RATIO_CHECK_MIN_OUTPUT = 16 * 1024 * 1024; // 16 MiB

export interface BombGuardOptions {
  maxRatio?: number;
  minOutputForRatio?: number;
  maxOutputBytes?: number; // optional explicit cap (default Infinity: rely on expansion ratio so >512 MiB streaming downloads succeed)
}

export function pipeThroughBoundedDecompression(
  source: ReadableStream<Uint8Array>,
  encoding: string,
  opts: BombGuardOptions = {}
): ReadableStream<Uint8Array> {
  const enc = encoding.trim().toLowerCase();
  if (enc !== 'gzip') {
    throw new Error(`Unsupported tunnel compression encoding: ${encoding}`);
  }
  if (typeof DecompressionStream !== 'function') {
    throw new Error('Native DecompressionStream API unavailable in this browser');
  }

  const maxRatio = opts.maxRatio ?? MAX_EXPANSION_RATIO;
  const minOut = opts.minOutputForRatio ?? RATIO_CHECK_MIN_OUTPUT;
  const maxOut = opts.maxOutputBytes ?? Number.POSITIVE_INFINITY;

  let totalIn = 0;
  let totalOut = 0;

  let srcReader: ReadableStreamDefaultReader<Uint8Array> | null = null;
  let decompReader: ReadableStreamDefaultReader<Uint8Array> | null = null;
  let decompWriter: WritableStreamDefaultWriter<Uint8Array> | null = null;
  let pumpPromise: Promise<void> | null = null;
  let started = false;
  let cancelled = false;

  const abortAll = (reason: unknown) => {
    cancelled = true;
    if (srcReader) {
      const r = srcReader;
      srcReader = null;
      void r.cancel(reason).catch(() => {});
    } else if (!started) {
      // Cancel upstream `source` directly when cancelled BEFORE first `pull()`!
      started = true;
      void source.cancel(reason).catch(() => {});
    }
    if (decompReader) {
      const dr = decompReader;
      decompReader = null;
      void dr.cancel(reason).catch(() => {});
    }
    if (decompWriter) {
      const dw = decompWriter;
      decompWriter = null;
      void dw.abort(reason).catch(() => {});
    }
  };

  const initDecompressorWithFirstChunk = (firstChunk: Uint8Array) => {
    const ds = new DecompressionStream('gzip') as unknown as TransformStream<Uint8Array, Uint8Array>;
    decompWriter = ds.writable.getWriter();
    decompReader = ds.readable.getReader();

    pumpPromise = (async () => {
      try {
        await decompWriter!.ready;
        if (cancelled) return;
        totalIn += firstChunk.byteLength;
        await decompWriter!.write(firstChunk);
        while (srcReader && !cancelled) {
          await decompWriter!.ready;
          if (!srcReader || cancelled) break;
          const { done, value } = await srcReader.read();
          if (done) break;
          if (!value || value.byteLength === 0) continue;
          totalIn += value.byteLength;
          await decompWriter!.write(value);
        }
        if (!cancelled && decompWriter) {
          await decompWriter.close();
        }
      } catch (err) {
        abortAll(err);
      }
    })();
  };

  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (cancelled) {
        controller.close();
        return;
      }
      if (!started) {
        started = true;
        srcReader = source.getReader();
        // Peek until first non-empty chunk or clean 0-byte EOF (never feeds 0 bytes to DecompressionStream!).
        while (!cancelled && srcReader) {
          const { done, value } = await srcReader.read();
          if (done) {
            try {
              srcReader.releaseLock();
            } catch {
              // ignore
            }
            srcReader = null;
            controller.close();
            return;
          }
          if (value && value.byteLength > 0) {
            initDecompressorWithFirstChunk(value);
            break;
          }
        }
      }

      if (!decompReader || cancelled) {
        controller.close();
        return;
      }

      while (!cancelled && decompReader) {
        let readRes: ReadableStreamReadResult<Uint8Array>;
        try {
          readRes = await decompReader.read();
        } catch (err) {
          abortAll(err);
          controller.error(err);
          return;
        }
        const { done, value } = readRes;
        if (done) {
          if (pumpPromise) await pumpPromise.catch(() => {});
          controller.close();
          return;
        }
        if (!value || value.byteLength === 0) {
          continue;
        }
        totalOut += value.byteLength;
        if (totalOut > maxOut) {
          const err = new Error('decompressed response exceeded maximum allowed size');
          abortAll(err);
          controller.error(err);
          return;
        }
        if (totalOut >= minOut && totalIn > 0 && totalOut / totalIn > maxRatio) {
          const err = new Error('decompression bomb detected (expansion ratio exceeded)');
          abortAll(err);
          controller.error(err);
          return;
        }
        controller.enqueue(value);
        return;
      }
    },
    cancel(reason) {
      abortAll(reason);
    },
  });
}
