/**
 * Simple method to get the type of a function.
 */
export type ObjectOrFunction<TOutput, TArgs> =
  | TOutput
  | ((args: TArgs) => TOutput);
