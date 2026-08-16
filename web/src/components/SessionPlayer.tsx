import { useEffect, useRef } from "react";
import * as AsciinemaPlayer from "asciinema-player";

import "asciinema-player/dist/bundle/asciinema-player.css";

/** Plays back a recorded shell session.
 *
 * Every session has been recorded to asciicast v2 since M3 and nothing could
 * read one back, so the files were evidence you needed shell access to the
 * server to look at. This is the other half.
 *
 * The player is imperative and owns its own DOM, so it is created in an effect
 * and disposed on unmount. Without the dispose it keeps a requestAnimationFrame
 * loop and an audio-less clock running after the element is gone, which shows
 * up as a tab that never goes idle.
 */
export function SessionPlayer({
  src,
  cols,
  rows,
}: {
  src: string;
  cols?: number;
  rows?: number;
}) {
  const host = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!host.current) return;
    const player = AsciinemaPlayer.create(
      // fetchOpts belongs on the SOURCE, not the options: the recording route
      // is authenticated like everything else and the player does its own
      // fetch, so the session cookie has to be asked for here.
      { url: src, fetchOpts: { credentials: "same-origin" } },
      host.current,
      {
        cols,
        rows,
        idleTimeLimit: 2, // long think-pauses are not worth watching in real time
        theme: "asciinema",
        // "both" inside a fixed-height box. With "width" the player scales the
        // font so 80 columns fill the container, which for a 24-row terminal
        // makes the player taller than the viewport and pushes everything else
        // off the page.
        fit: "both",
      },
    );
    return () => player.dispose();
  }, [src, cols, rows]);

  return <div ref={host} className="h-[26rem] overflow-hidden rounded-lg border" />;
}
