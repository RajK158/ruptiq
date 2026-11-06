// Ruptiq HTTP GET workload for k6.
//
// This file is static: Ruptiq never edits or generates it. Every input comes
// from environment variables set by the Ruptiq k6 runner, and the load shape
// (virtual users, duration) comes from k6 command-line flags.
//
//   RUPTIQ_TARGET_URL  absolute http or https URL to request (required)
import http from 'k6/http';

const target = __ENV.RUPTIQ_TARGET_URL;
if (!target) {
  throw new Error('RUPTIQ_TARGET_URL is not set');
}

const params = {
  // Bounds a single request, so a stuck target cannot hold the run open long
  // after its duration (k6's own default is 60s).
  timeout: '10s',
  // One fixed name keeps k6 from tracking a separate series per URL.
  tags: { name: 'ruptiq_target' },
};

export default function () {
  http.get(target, params);
}
