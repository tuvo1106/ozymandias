/**
 * The public integration protocol (M5 spec section 2). An integration is the
 * only place that knows about a library; it uses nothing but the public
 * tracer API, so a third party can write one with exactly the same power as
 * the built-in ones.
 *
 * @module
 */

/** A patch for one library or framework seam. */
export interface Integration {
  /** Unique name; `init({integrations: [name]})` selects it. */
  readonly name: string;
  /** Whether the target is present and patchable. Must not throw. */
  isAvailable(): boolean;
  /** Installs the instrumentation. Idempotent. */
  patch(): void;
  /** Removes it, restoring the original behaviour. */
  unpatch(): void;
}
