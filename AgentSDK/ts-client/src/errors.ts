export class SocktError extends Error {
  slug?: string;
  statusCode?: number;
  constructor(message: string, opts?: { slug?: string; statusCode?: number }) {
    super(message);
    this.name = new.target.name;
    this.slug = opts?.slug;
    this.statusCode = opts?.statusCode;
  }
}

export class AuthenticationError extends SocktError {}
export class SandboxNotRunningError extends SocktError {}
export class TopUpFailedError extends SocktError {}
export class NoCapacityError extends SocktError {}
export class HostUnreachableError extends SocktError {}
export class APIError extends SocktError {}
