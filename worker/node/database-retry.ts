const transientCodes = new Set([
  "ECONNREFUSED",
  "ECONNRESET",
  "ETIMEDOUT",
  "EPIPE",
  "EAI_AGAIN",
  "08000",
  "08001",
  "08003",
  "08006",
  "57P01",
  "57P02",
  "57P03",
]);

// Only use for idempotent initialization, never replay a transaction or user command.
export async function retryDatabaseInitialization<T>(
  initialize: () => Promise<T>,
  options: {
    attempts?: number;
    sleep?: (milliseconds: number) => Promise<void>;
    warn?: (message: string) => void;
  } = {},
): Promise<T> {
  const attempts = options.attempts ?? 6;
  const sleep = options.sleep ?? ((ms) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
  const warn = options.warn ?? console.warn;
  const attemptInitialization = async (attempt: number): Promise<T> => {
    try {
      return await initialize();
    } catch (error) {
      const code = error && typeof error === "object" && "code" in error ? error.code : undefined;
      if (attempt >= attempts || typeof code !== "string" || !transientCodes.has(code)) throw error;
      const delay = Math.min(10_000, 1_000 * 2 ** (attempt - 1));
      // Do not log database errors: messages can contain connection credentials.
      warn(
        `coordinator database unavailable; initialization retry ${attempt}/${attempts} in ${delay}ms`,
      );
      await sleep(delay);
      return attemptInitialization(attempt + 1);
    }
  };
  return attemptInitialization(1);
}
