import { ethers } from 'ethers';
import { SiweMessage } from 'siwe';

async function generateSiweMessage() {
    // Generate a random private key
    const wallet = ethers.Wallet.createRandom();
    console.log('Generated Ethereum address:', wallet.address);

    // Create a SIWE message
    const domain = 'web3-project.xyz';
    const origin = 'https://frontend.web3-project.xyz';
    
    const message = new SiweMessage({
        domain,
        address: wallet.address,
        statement: 'Sign in with Ethereum to the app.',
        uri: origin,
        version: '1',
        chainId: 1,
        nonce: '11235436457567345234',
    });

    // Create the message string
    const messageString = message.prepareMessage();

    // Sign the message
    const signature = await wallet.signMessage(messageString);

    // Encode the message and signature
    const base64Message = Buffer.from(messageString).toString('base64');
    const hexSignature = signature;

    console.log('X-SIWE-Message:', base64Message);
    console.log('X-SIWE-Signature:', hexSignature);
}

generateSiweMessage().catch(console.error);