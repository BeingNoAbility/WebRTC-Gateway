/**
 * V7 Native Bounded Streaming Gzip Decompression Pipeline
 * - Peeks first non-empty chunk BEFORE constructing `DecompressionStream('gzip')` so
 *   0-byte empty bodies close cleanly instead of throwing `TypeError` on empty gzip EOF!
 * - Tuned bomb guard (`minOutput = 16 MiB`, `maxRatio = 1024`, `maxOutput = 512 MiB`) so
 *   highly compressible legitimate assets (sparse JSON/CSV/SQL dumps) pass while real bombs abort.
 */

const MAX_EXPANSION_RATIO = 1024;
const RATIO_CHECK_MIN_OUTPUT = 16 * 1024 * 1024; // 16 MiB
const MAX_DECOMPRESSED_BYTES = 512 * 1024 * 1024; // 512 MiB

export interface BombGuardOptions {
  maxRatio?: number;
  minOutputForRatio?: number;
  maxOutputBytes?: number;
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
  const maxOut = opts.maxOutputBytes ?? MAX_DECOMPRESSED_BYTES;

  let totalIn = 0;
  let totalOut = 0;

  let srcReader: ReadableStreamDefaultReader<Uint8Array> | null = null;
  let decompReader: ReadableStreamDefaultReader<Uint8Array> | null = null;
  let decompWriter: WritableStreamDefaultWriter<Uint8Array> | null = null;
  let pumpPromise: Promise<void> | null = null;
  let started = false;

  const initDecompressorWithFirstChunk = (firstChunk: Uint8Array) => {
    const ds = new DecompressionStream('gzip') as unknown as TransformStream<Uint8Array, Uint8Array>;
    decompWriter = ds.writable.getWriter();
    decompReader = ds.readable.getReader();

    pumpPromise = (async () => {
      try {
        totalIn += firstChunk.byteLength;
        await decompWriter!.write(firstChunk);
        while (srcReader) {
          const { done, value } = await srcReader.read();
          if (done) break;
          if (!value || value.byteLength === 0) continue;
          totalIn += value.byteLength;
          await decompWriter!.write(value);
        }
        await decompWriter!.close();
      } catch (err) {
        // Cancel upstream source immediately so corrupt gzip never pins an HTTP pool slot!
        void srcReader?.cancel(err).catch(() => {});
        await decompWriter!.abort(err).catch(() => {});
      }
    })();
  };

  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (!started) {
        started = true;
        srcReader = source.getReader();
        // Peek until first non-empty chunk or clean 0-byte EOF (never feeds 0 bytes to DecompressionStream!).
        while (true) {
          const { done, value } = await srcReader.read();
          if (done) {
            srcReader.releaseLock();
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

      if (!decompReader) {
        controller.close();
        return;
      }

      // Loop over 0-byte decompressed chunks so pull() NEVER returns without enqueuing or closing!
      while (true) {
        let readRes: ReadableStreamReadResult<Uint8Array>;
        try {
          readRes = await decompReader.read();
        } catch (err) {
          void srcReader?.cancel(err).catch(() => {});
          void decompWriter?.abort(err).catch(() => {});
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
          void srcReader?.cancel(err).catch(() => {});
          void decompReader.cancel(err).catch(() => {});
          controller.error(err);
          return;
        }
        if (totalOut >= minOut && totalIn > 0 && totalOut / totalIn > maxRatio) {
          const err = new Error('decompression bomb detected (expansion ratio exceeded)');
          void srcReader?.cancel(err).catch(() => {});
          void decompReader.cancel(err).catch(() => {});
          controller.error(err);
          return;
        }
        controller.enqueue(value);
        return;
      }
    },
    cancel(reason) {
      void srcReader?.cancel(reason).catch(() => {});
      void decompReader?.cancel(reason).catch(() => {});
      void decompWriter?.abort(reason).catch(() => {});
    },
  });
}
