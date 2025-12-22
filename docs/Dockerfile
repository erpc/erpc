# Use the official Node.js image as the base image
FROM node:20-alpine@sha256:658d0f6c3b0fe0a4edc78d6f68f2bf8f2925f43db3ea81e1df01de6f24c2a99d

# Install pnpm
RUN npm install -g pnpm

# Set the working directory to /app
WORKDIR /app

# Copy the package.json and pnpm-lock.yaml files to the working directory
COPY package.json pnpm-lock.yaml ./

# Install dependencies using pnpm
RUN pnpm install 

# Copy the rest of your application code to the working directory
COPY . .

# Build the Next.js application
RUN pnpm build

# Expose port 3000 to the outside world
EXPOSE 3000

# Command to run the Next.js application
CMD ["pnpm", "start"]
