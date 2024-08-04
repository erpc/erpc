import * as crypto from 'node:crypto';
import * as jwt from 'jsonwebtoken';
import * as fs from 'node:fs';

// Define types for our JWT payload
interface JwtPayload {
  iss: string;
  aud: string;
  exp: number;
  iat: number;
  sub: string;
  role: string;
  name: string;
}

// Generate RSA key pair
const { privateKey, publicKey } = crypto.generateKeyPairSync('rsa', {
  modulusLength: 2048,
  publicKeyEncoding: {
    type: 'spki',
    format: 'pem'
  },
  privateKeyEncoding: {
    type: 'pkcs8',
    format: 'pem'
  }
});

// Create JWT payload
const payload: JwtPayload = {
  iss: 'https://erpc.web3-project.xyz',
  aud: 'https://frontend.web3-project.xyz',
  exp: 1999999999999999,
  iat: Math.floor(Date.now() / 1000),
  sub: '1234567890',
  role: 'degen',
  name: 'Abu Vitalik'
};

// Sign the JWT
const token = jwt.sign(payload, privateKey, { 
  algorithm: 'RS256',
  header: {
    alg: 'RS256',
    kid: "rsa-key-1"
  }
});

// Output the results
console.log("Public Key (add this to your config):");
console.log("-------------------------------------");
console.log(publicKey);
console.log("-------------------------------------");

console.log("\nJWT Token (use this for testing):");
console.log("-------------------------------------");
console.log(token);
console.log("-------------------------------------");

// Save the public key to a file
fs.writeFileSync('public_key.pem', publicKey);
console.log("\nPublic key has been saved to 'public_key.pem'");

// Verify the token (for demonstration purposes)
try {
  const decoded = jwt.verify(token, publicKey, { algorithms: ['RS256'] }) as JwtPayload;
  console.log("\nToken verified successfully. Decoded payload:");
  console.log(JSON.stringify(decoded, null, 2));
} catch (err) {
  if (err instanceof Error) {
    console.error("Token verification failed:", err.message);
  } else {
    console.error("An unknown error occurred during token verification");
  }
}