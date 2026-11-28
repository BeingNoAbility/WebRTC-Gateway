/**
 * V7 Monotonic WebRTC Telemetry Collector
 * - Re-entrancy guarded (`polling` flag prevents overlapping getStats calls).
 * - Generation-guarded against firing `onUpdate` after `stop()`.
 * - Resolves active candidate-pair across Chromium (`transport.selectedCandidatePairId`)
 *   and Firefox (`candidate-pair.selected === true` or `state === 'succeeded' && nominated`).
 */

import type { PoolMux } from '../../protocol/mux';

export interface TunnelStatsSnapshot {
  rttMs: number | null;
  txBps: number;
  rxBps: number;
  activeStreams: number;
  candidateType: string;
  poolSize: number;
}

export class StatsCollector {
  private timer: ReturnType<typeof setInterval> | null = null;
  private polling = false;
  private generation = 0;
  private lastTs = 0;
  private lastBytesSent = 0;
  private lastBytesRecv = 0;

  start(
    getPC: () => RTCPeerConnection | null,
    getMux: () => PoolMux | null,
    onUpdate: (snap: TunnelStatsSnapshot) => void,
    intervalMs = 1_500
  ): void {
    this.stop();
    const myGen = ++this.generation;
    this.lastTs = Date.now();
    this.lastBytesSent = 0;
    this.lastBytesRecv = 0;

    this.timer = setInterval(async () => {
      if (this.polling || myGen !== this.generation) return;
      const pc = getPC();
      const mux = getMux();
      if (!pc || !mux) return;

      this.polling = true;
      try {
        let rttMs: number | null = null;
        let bytesSent = 0;
        let bytesRecv = 0;
        let candidateType = 'unknown';

        try {
          const report = await pc.getStats();
          if (myGen !== this.generation) return;
          let selectedPairId = '';
          let activeLocalId = '';
          const localCandidates = new Map<string, string>();
          const pairs = new Map<
            string,
            {
              state?: string;
              nominated?: boolean;
              selected?: boolean;
              currentRoundTripTime?: number;
              bytesSent?: number;
              bytesReceived?: number;
              localCandidateId?: string;
            }
          >();

          report.forEach((stat) => {
            if (stat.type === 'local-candidate') {
              const c = stat as unknown as { id: string; candidateType?: string };
              if (c.candidateType) localCandidates.set(c.id, c.candidateType);
            } else if (stat.type === 'transport') {
              const t = stat as unknown as {
                bytesSent?: number;
                bytesReceived?: number;
                selectedCandidatePairId?: string;
              };
              if (typeof t.bytesSent === 'number') bytesSent = t.bytesSent;
              if (typeof t.bytesReceived === 'number') bytesRecv = t.bytesReceived;
              if (t.selectedCandidatePairId) selectedPairId = t.selectedCandidatePairId;
            } else if (stat.type === 'candidate-pair') {
              const p = stat as unknown as {
                id: string;
                state?: string;
                nominated?: boolean;
                selected?: boolean;
                currentRoundTripTime?: number;
                bytesSent?: number;
                bytesReceived?: number;
                localCandidateId?: string;
              };
              pairs.set(p.id, p);
            }
          });

          const pairList = Array.from(pairs.values());
          const chosenPair =
            (selectedPairId ? pairs.get(selectedPairId) : undefined) ||
            pairList.find((p) => p.selected === true) ||
            pairList.find((p) => p.state === 'succeeded' && p.nominated) ||
            pairList.find((p) => p.state === 'succeeded');

          if (chosenPair) {
            if (typeof chosenPair.currentRoundTripTime === 'number') {
              rttMs = Math.round(chosenPair.currentRoundTripTime * 1000);
            }
            if (bytesSent === 0 && typeof chosenPair.bytesSent === 'number') bytesSent = chosenPair.bytesSent;
            if (bytesRecv === 0 && typeof chosenPair.bytesReceived === 'number') bytesRecv = chosenPair.bytesReceived;
            if (chosenPair.localCandidateId) activeLocalId = chosenPair.localCandidateId;
          }

          if (activeLocalId && localCandidates.has(activeLocalId)) {
            candidateType = localCandidates.get(activeLocalId)!;
          }
        } catch {
          // ignore transient getStats error
        }

        if (myGen !== this.generation) return;
        const now = Date.now();
        const dtSec = Math.max(0.2, (now - this.lastTs) / 1000);
        const txBps = bytesSent >= this.lastBytesSent ? (bytesSent - this.lastBytesSent) / dtSec : 0;
        const rxBps = bytesRecv >= this.lastBytesRecv ? (bytesRecv - this.lastBytesRecv) / dtSec : 0;
        this.lastTs = now;
        this.lastBytesSent = bytesSent;
        this.lastBytesRecv = bytesRecv;

        onUpdate({
          rttMs,
          txBps,
          rxBps,
          activeStreams: mux.activeStreamCount,
          candidateType,
          poolSize: mux.poolSize,
        });
      } finally {
        this.polling = false;
      }
    }, intervalMs);
  }

  stop(): void {
    this.generation++;
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = null;
    }
    this.polling = false;
  }
}
