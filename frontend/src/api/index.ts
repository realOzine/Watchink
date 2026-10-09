import { platform } from '../platform';
import { createClient } from './client';

/** The single HTTP client shared by every page, in both modes. */
export const api = createClient(platform);

export { ApiError, NetworkError } from './errors';
export type { ApiClient, FullNote, FullTranscript, ImportInput, SettingsPatch } from './client';
