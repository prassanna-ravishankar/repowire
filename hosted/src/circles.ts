// A port of proto.SharedCircle (daemon-go/proto/remote.go), pinned by the
// shared vectors in daemon-go/proto/testdata/shared_circles.json. A shared
// circle spans the owner's machines; a reserved one never leaves its machine.
const digits = (s: string) => s !== '' && /^[0-9]+$/.test(s);

export function sharedCircle(circle: string): boolean {
  if (circle === '' || circle === '*' || circle === 'default' || digits(circle)) return false;
  if (circle.startsWith('window-') && digits(circle.slice('window-'.length))) return false;
  return true;
}

export const DEFAULT_CIRCLE = 'managed';
