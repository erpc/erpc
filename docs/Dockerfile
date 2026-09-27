# Use the official Node.js image as the base image
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293

# Install pnpm
RUN npm install -g pnpm@10.28.2

# Set the working directory to /app
WORKDIR /app

# Copy dependency manifests and build-script policy before installing
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./

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
